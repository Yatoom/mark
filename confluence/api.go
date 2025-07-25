package confluence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kovetskiy/gopencils"
	"github.com/kovetskiy/lorg"
	"github.com/reconquest/karma-go"
	"github.com/reconquest/pkg/log"
)

type User struct {
	AccountID string `json:"accountId,omitempty"`
	UserKey   string `json:"userKey,omitempty"`
}

type API struct {
	rest *gopencils.Resource

	// it's deprecated accordingly to Atlassian documentation,
	// but it's only way to set permissions
	json    *gopencils.Resource
	BaseURL string
}

type SpaceInfo struct {
	ID   int    `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`

	Homepage PageInfo `json:"homepage"`

	Links struct {
		Full string `json:"webui"`
	} `json:"_links"`
}

type PageInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`

	Version struct {
		Number  int64  `json:"number"`
		Message string `json:"message"`
	} `json:"version"`

	Ancestors []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"ancestors"`

	Links struct {
		Full string `json:"webui"`
	} `json:"_links"`
}

type AttachmentInfo struct {
	Filename string `json:"title"`
	ID       string `json:"id"`
	Metadata struct {
		Comment string `json:"comment"`
	} `json:"metadata"`
	Links struct {
		Context  string `json:"context"`
		Download string `json:"download"`
	} `json:"_links"`
}

type Label struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Name   string `json:"name"`
}
type LabelInfo struct {
	Labels []Label `json:"results"`
	Size   int     `json:"number"`
}
type form struct {
	buffer io.Reader
	writer *multipart.Writer
}

type tracer struct {
	prefix string
}

func (tracer *tracer) Printf(format string, args ...interface{}) {
	log.Tracef(nil, tracer.prefix+" "+format, args...)
}

func NewAPI(baseURL string, username string, password string) *API {
	var auth *gopencils.BasicAuth
	if username != "" {
		auth = &gopencils.BasicAuth{
			Username: username,
			Password: password,
		}
	}
	rest := gopencils.Api(baseURL+"/rest/api", auth, 3) // set option for 3 retries on failure
	if username == "" {
		if rest.Headers == nil {
			rest.Headers = http.Header{}
		}
		rest.SetHeader("Authorization", fmt.Sprintf("Bearer %s", password))
	}

	json := gopencils.Api(baseURL+"/rpc/json-rpc/confluenceservice-v2", auth, 3)

	if log.GetLevel() == lorg.LevelTrace {
		rest.Logger = &tracer{"rest:"}
		json.Logger = &tracer{"json-rpc:"}
	}

	return &API{
		rest:    rest,
		json:    json,
		BaseURL: strings.TrimSuffix(baseURL, "/"),
	}
}

// doWithRetry executes fn up to attempts times while the returned
// *http.Response has status 429 or 5xx.
// It applies exponential back-off with jitter between retries.
func doWithRetry(
	ctx context.Context,
	attempts int,
	fn func() (*http.Response, error),
) (*http.Response, error) {
	var (
		resp *http.Response
		err  error
	)

	// 1s, 2s, 4s … with ±25 % jitter
	base := time.Second
	for i := 0; i < attempts; i++ {
		if i > 0 {
			jitter := time.Duration(rand.Int63n(int64(base/4))) - base/8
			sleep := base + jitter
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			base *= 2
		}

		resp, err = fn()
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Fully drain body so the connection can be re-used.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	return resp, karma.Describe("attempts", attempts).Reason(
		"exceeded max retries for 429 (Too Many Requests) status code",
	)
}

func (api *API) FindRootPage(space string) (*PageInfo, error) {
	page, err := api.FindPage(space, ``, "page")
	if err != nil {
		return nil, karma.Format(
			err,
			"can't obtain first page from space %q",
			space,
		)
	}

	if page == nil {
		return nil, errors.New("no such space")
	}

	if len(page.Ancestors) == 0 {
		return &PageInfo{
			ID:    page.ID,
			Title: page.Title,
		}, nil
	}

	return &PageInfo{
		ID:    page.Ancestors[0].ID,
		Title: page.Ancestors[0].Title,
	}, nil
}

func (api *API) FindHomePage(space string) (*PageInfo, error) {
	var result SpaceInfo
	payload := map[string]string{
		"expand": "homepage",
	}

	reqFn := func() (*http.Response, error) {
		req, err := api.rest.Res("space/"+space, &result).Get(payload)
		if err != nil {
			return nil, err
		}
		return req.Raw, nil
	}
	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.FindHomePage(space)
	}

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	return &result.Homepage, nil
}

func (api *API) FindPage(
	space string,
	title string,
	pageType string,
) (*PageInfo, error) {
	result := struct {
		Results []PageInfo `json:"results"`
	}{}

	payload := map[string]string{
		"spaceKey": space,
		"expand":   "ancestors,version",
		"type":     pageType,
	}

	if title != "" {
		payload["title"] = title
	}

	reqFn := func() (*http.Response, error) {
		req, err := api.rest.Res(
			"content/", &result,
		).Get(payload)
		if err != nil {
			return nil, err
		}
		return req.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.FindPage(space, title, pageType)
	}

	// allow 404 because it's fine if page is not found,
	// the function will return nil, nil
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	if len(result.Results) == 0 {
		return nil, nil
	}

	return &result.Results[0], nil
}

func (api *API) CreateAttachment(
	pageID string,
	name string,
	comment string,
	reader io.Reader,
) (AttachmentInfo, error) {
	var info AttachmentInfo

	form, err := getAttachmentPayload(name, comment, reader)
	if err != nil {
		return AttachmentInfo{}, err
	}

	var result struct {
		Links struct {
			Context string `json:"context"`
		} `json:"_links"`
		Results []AttachmentInfo `json:"results"`
	}

	resource := api.rest.Res(
		"content/"+pageID+"/child/attachment", &result,
	)

	resource.Payload = form.buffer
	oldHeaders := resource.Headers.Clone()
	resource.Headers = http.Header{}
	if resource.Api.BasicAuth == nil {
		resource.Headers.Set("Authorization", oldHeaders.Get("Authorization"))
	}

	resource.SetHeader("Content-Type", form.writer.FormDataContentType())
	resource.SetHeader("X-Atlassian-Token", "no-check")

	reqFn := func() (*http.Response, error) {
		request, err := resource.Post()
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return info, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.CreateAttachment(pageID, name, comment, reader)
	}

	if resp.StatusCode != http.StatusOK {
		return info, newErrorStatus(resp)
	}

	if len(result.Results) == 0 {
		return info, errors.New(
			"the Confluence REST API for creating attachments returned " +
				"0 json objects, expected at least 1",
		)
	}

	for i, info := range result.Results {
		if info.Links.Context == "" {
			info.Links.Context = result.Links.Context
		}

		result.Results[i] = info
	}

	info = result.Results[0]

	return info, nil
}

// UpdateAttachment uploads a new version of the same attachment if the
// checksums differs from the previous one.
// It also handles a case where Confluence returns sort of "short" variant of
// the response instead of an extended one.
func (api *API) UpdateAttachment(
	pageID string,
	attachID string,
	name string,
	comment string,
	reader io.Reader,
) (AttachmentInfo, error) {
	var info AttachmentInfo

	form, err := getAttachmentPayload(name, comment, reader)
	if err != nil {
		return AttachmentInfo{}, err
	}

	var extendedResponse struct {
		Links struct {
			Context string `json:"context"`
		} `json:"_links"`
		Results []AttachmentInfo `json:"results"`
	}

	var result json.RawMessage

	resource := api.rest.Res(
		"content/"+pageID+"/child/attachment/"+attachID+"/data", &result,
	)

	resource.Payload = form.buffer
	oldHeaders := resource.Headers.Clone()
	resource.Headers = http.Header{}
	if resource.Api.BasicAuth == nil {
		resource.Headers.Set("Authorization", oldHeaders.Get("Authorization"))
	}

	resource.SetHeader("Content-Type", form.writer.FormDataContentType())
	resource.SetHeader("X-Atlassian-Token", "no-check")

	reqFn := func() (*http.Response, error) {
		request, err := resource.Post()
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return info, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.UpdateAttachment(pageID, attachID, name, comment, reader)
	}

	if resp.StatusCode != http.StatusOK {
		return info, newErrorStatus(resp)
	}

	err = json.Unmarshal(result, &extendedResponse)
	if err != nil {
		return info, karma.Format(
			err,
			"unable to unmarshal JSON response as full response format: %s",
			string(result),
		)
	}

	if len(extendedResponse.Results) > 0 {
		for i, info := range extendedResponse.Results {
			if info.Links.Context == "" {
				info.Links.Context = extendedResponse.Links.Context
			}

			extendedResponse.Results[i] = info
		}

		info = extendedResponse.Results[0]

		return info, nil
	}

	var shortResponse AttachmentInfo
	err = json.Unmarshal(result, &shortResponse)
	if err != nil {
		return info, karma.Format(
			err,
			"unable to unmarshal JSON response as short response format: %s",
			string(result),
		)
	}

	return shortResponse, nil
}

func getAttachmentPayload(name, comment string, reader io.Reader) (*form, error) {
	var (
		payload = bytes.NewBuffer(nil)
		writer  = multipart.NewWriter(payload)
	)

	content, err := writer.CreateFormFile("file", name)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to create form file",
		)
	}

	_, err = io.Copy(content, reader)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to copy i/o between form-file and file",
		)
	}

	commentWriter, err := writer.CreateFormField("comment")
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to create form field for comment",
		)
	}

	_, err = commentWriter.Write([]byte(comment))
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to write comment in form-field",
		)
	}

	err = writer.Close()
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to close form-writer",
		)
	}

	return &form{
		buffer: payload,
		writer: writer,
	}, nil
}

func (api *API) GetAttachments(pageID string) ([]AttachmentInfo, error) {
	result := struct {
		Links struct {
			Context string `json:"context"`
		} `json:"_links"`
		Results []AttachmentInfo `json:"results"`
	}{}

	payload := map[string]string{
		"expand": "version,container",
		"limit":  "1000",
	}

	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+pageID+"/child/attachment", &result,
		).Get(payload)
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.GetAttachments(pageID)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	for i, info := range result.Results {
		if info.Links.Context == "" {
			info.Links.Context = result.Links.Context
		}

		result.Results[i] = info
	}

	return result.Results, nil
}

func (api *API) GetPageByID(pageID string) (*PageInfo, error) {

	var page PageInfo
	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+pageID, &page,
		).Get(map[string]string{"expand": "ancestors,version"})
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.GetPageByID(pageID)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	return &page, nil
}

func (api *API) CreatePage(
	space string,
	pageType string,
	parent *PageInfo,
	title string,
	body string,
) (*PageInfo, error) {
	payload := map[string]interface{}{
		"type":  pageType,
		"title": title,
		"space": map[string]interface{}{
			"key": space,
		},
		"body": map[string]interface{}{
			"storage": map[string]interface{}{
				"representation": "storage",
				"value":          body,
			},
		},
		"metadata": map[string]interface{}{
			"properties": map[string]interface{}{
				"editor": map[string]interface{}{
					"value": "v2",
				},
			},
		},
	}

	if parent != nil {
		payload["ancestors"] = []map[string]interface{}{
			{"id": parent.ID},
		}
	}

	var page PageInfo
	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/", &page,
		).Post(payload)
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.CreatePage(space, pageType, parent, title, body)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	return &page, nil
}

func (api *API) UpdatePage(page *PageInfo, newContent string, minorEdit bool, versionMessage string, newLabels []string, appearance string, emojiString string) error {
	nextPageVersion := page.Version.Number + 1
	oldAncestors := []map[string]interface{}{}

	if page.Type != "blogpost" && len(page.Ancestors) > 0 {
		// picking only the last one, which is required by confluence
		oldAncestors = []map[string]interface{}{
			{"id": page.Ancestors[len(page.Ancestors)-1].ID},
		}
	}

	properties := map[string]interface{}{
		// Fix to set full-width as has changed on Confluence APIs again.
		// https://jira.atlassian.com/browse/CONFCLOUD-65447
		//
		"content-appearance-published": map[string]interface{}{
			"value": appearance,
		},
		// content-appearance-draft should not be set as this is impacted by
		// the user editor default configurations - which caused the sporadic published widths.
	}

	if emojiString != "" {
		r, _ := utf8.DecodeRuneInString(emojiString)
		unicodeHex := fmt.Sprintf("%x", r)

		properties["emoji-title-draft"] = map[string]interface{}{
			"value": unicodeHex,
		}
		properties["emoji-title-published"] = map[string]interface{}{
			"value": unicodeHex,
		}
	}

	payload := map[string]interface{}{
		"id":    page.ID,
		"type":  page.Type,
		"title": page.Title,
		"version": map[string]interface{}{
			"number":    nextPageVersion,
			"minorEdit": minorEdit,
			"message":   versionMessage,
		},
		"ancestors": oldAncestors,
		"body": map[string]interface{}{
			"storage": map[string]interface{}{
				"value":          newContent,
				"representation": "storage",
			},
		},
		"metadata": map[string]interface{}{
			"properties": properties,
		},
	}

	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+page.ID, &map[string]interface{}{},
		).Put(payload)
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.UpdatePage(page, newContent, minorEdit, versionMessage, newLabels, appearance, emojiString)
	}

	if resp.StatusCode != http.StatusOK {
		return newErrorStatus(resp)
	}

	return nil
}

func (api *API) AddPageLabels(page *PageInfo, newLabels []string) (*LabelInfo, error) {

	labels := []map[string]interface{}{}
	for _, label := range newLabels {
		if label != "" {
			item := map[string]interface{}{
				"prefix": "global",
				"name":   label,
			}
			labels = append(labels, item)
		}
	}

	payload := labels

	var labelInfo LabelInfo
	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+page.ID+"/label", &labelInfo,
		).Post(payload)
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.AddPageLabels(page, newLabels)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}

	return &labelInfo, nil
}

func (api *API) DeletePageLabel(page *PageInfo, label string) (*LabelInfo, error) {

	var labelInfo LabelInfo
	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+page.ID+"/label", &labelInfo,
		).SetQuery(map[string]string{"name": label}).Delete()
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.DeletePageLabel(page, label)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, newErrorStatus(resp)
	}

	return &labelInfo, nil
}

func (api *API) GetPageLabels(page *PageInfo, prefix string) (*LabelInfo, error) {

	var labelInfo LabelInfo
	reqFn := func() (*http.Response, error) {
		request, err := api.rest.Res(
			"content/"+page.ID+"/label", &labelInfo,
		).Get(map[string]string{"prefix": prefix})
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.GetPageLabels(page, prefix)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newErrorStatus(resp)
	}
	return &labelInfo, nil
}

func (api *API) GetUserByName(name string) (*User, error) {
	var response struct {
		Results []struct {
			User User
		}
	}

	// Try the new path first
	_, err := api.rest.
		Res("search").
		Res("user", &response).
		Get(map[string]string{
			"cql": fmt.Sprintf("user.fullname~%q", name),
		})
	if err != nil {
		return nil, err
	}

	// Try old path
	if len(response.Results) == 0 {
		_, err := api.rest.
			Res("search", &response).
			Get(map[string]string{
				"cql": fmt.Sprintf("user.fullname~%q", name),
			})
		if err != nil {
			return nil, err
		}
	}

	if len(response.Results) == 0 {

		return nil, karma.
			Describe("name", name).
			Reason(
				"user with given name is not found",
			)
	}

	return &response.Results[0].User, nil
}

func (api *API) GetCurrentUser() (*User, error) {
	var user User

	_, err := api.rest.
		Res("user").
		Res("current", &user).
		Get()
	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (api *API) RestrictPageUpdatesCloud(
	page *PageInfo,
	allowedUser string,
) error {
	user, err := api.GetCurrentUser()
	if err != nil {
		return err
	}

	var result interface{}

	reqFn := func() (*http.Response, error) {
		request, err := api.rest.
			Res("content").
			Id(page.ID).
			Res("restriction", &result).
			Post([]map[string]interface{}{
				{
					"operation": "update",
					"restrictions": map[string]interface{}{
						"user": []map[string]interface{}{
							{
								"type":      "known",
								"accountId": user.AccountID,
							},
						},
					},
				},
			})
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.RestrictPageUpdatesCloud(page, allowedUser)
	}

	if resp.StatusCode != http.StatusOK {
		return newErrorStatus(resp)
	}

	return nil
}

func (api *API) RestrictPageUpdatesServer(
	page *PageInfo,
	allowedUser string,
) error {
	var (
		err    error
		result interface{}
	)

	reqFn := func() (*http.Response, error) {
		request, err := api.json.Res(
			"setContentPermissions", &result,
		).Post([]interface{}{
			page.ID,
			"Edit",
			[]map[string]interface{}{
				{
					"userName": allowedUser,
				},
			},
		})
		if err != nil {
			return nil, err
		}
		return request.Raw, nil
	}

	resp, err := doWithRetry(context.Background(), 5, reqFn)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1 * time.Second)
		return api.RestrictPageUpdatesServer(page, allowedUser)
	}

	if resp.StatusCode != http.StatusOK {
		return newErrorStatus(resp)
	}

	if success, ok := result.(bool); !ok || !success {
		return fmt.Errorf(
			"'true' response expected, but '%v' encountered",
			result,
		)
	}

	return nil
}

func (api *API) RestrictPageUpdates(
	page *PageInfo,
	allowedUser string,
) error {
	var err error

	if strings.HasSuffix(api.rest.Api.BaseUrl.Host, "jira.com") || strings.HasSuffix(api.rest.Api.BaseUrl.Host, "atlassian.net") {
		err = api.RestrictPageUpdatesCloud(page, allowedUser)
	} else {
		err = api.RestrictPageUpdatesServer(page, allowedUser)
	}

	return err
}

// newErrorStatus converts a non-2xx response into a useful error.
func newErrorStatus(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("the Confluence API returned 401 (Unauthorized)")
	case http.StatusNotFound:
		return errors.New("the Confluence API returned 404 (Not Found)")
	default:
		return fmt.Errorf("the Confluence API returned %s: %s", resp.Status, body)
	}
}
