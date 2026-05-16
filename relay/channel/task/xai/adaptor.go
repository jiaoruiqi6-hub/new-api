package xai

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

const ChannelName = "xai-video"

var ModelList = []string{
	"grok-video-3",
	"grok-video-3-max",
	"grok-video-3-pro",
	"grok-imagine-video",
}

type TaskAdaptor struct {
	taskcommon.BaseBilling
	apiKey  string
	baseURL string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.apiKey = info.ApiKey
	a.baseURL = strings.TrimRight(info.ChannelBaseUrl, "/")
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	return relaycommon.ValidateMultipartDirect(c, info)
}

func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	if useOpenAIVideoCreatePath(a.baseURL) {
		return fmt.Sprintf("%s/v1/videos", a.baseURL), nil
	}
	return fmt.Sprintf("%s/v1/videos/generations", a.baseURL), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, errors.Wrap(err, "get_request_body_failed")
	}
	cachedBody, err := storage.Bytes()
	if err != nil {
		return nil, errors.Wrap(err, "read_body_bytes_failed")
	}

	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var bodyMap map[string]any
		if err := common.Unmarshal(cachedBody, &bodyMap); err != nil {
			return bytes.NewReader(cachedBody), nil
		}
		bodyMap["model"] = info.UpstreamModelName
		newBody, err := common.Marshal(bodyMap)
		if err != nil {
			return nil, errors.Wrap(err, "marshal_request_body_failed")
		}
		return bytes.NewReader(newBody), nil
	}

	if strings.Contains(contentType, "multipart/form-data") {
		formData, err := common.ParseMultipartFormReusable(c)
		if err != nil {
			return bytes.NewReader(cachedBody), nil
		}
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		_ = writer.WriteField("model", info.UpstreamModelName)
		for key, values := range formData.Value {
			if key == "model" {
				continue
			}
			for _, v := range values {
				_ = writer.WriteField(key, v)
			}
		}
		for fieldName, fileHeaders := range formData.File {
			for _, fh := range fileHeaders {
				if err := copyMultipartFile(writer, fieldName, fh); err != nil {
					continue
				}
			}
		}
		_ = writer.Close()
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
		return &buf, nil
	}

	return common.ReaderOnly(storage), nil
}

func copyMultipartFile(writer *multipart.Writer, fieldName string, fh *multipart.FileHeader) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	contentType := fh.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		buf512 := make([]byte, 512)
		n, _ := io.ReadFull(f, buf512)
		contentType = http.DetectContentType(buf512[:n])
		_ = f.Close()
		f, err = fh.Open()
		if err != nil {
			return err
		}
		defer f.Close()
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, fh.Filename))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(part, f)
	return err
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	upstreamID := extractUpstreamTaskID(responseBody)
	if upstreamID == "" {
		return "", responseBody, service.TaskErrorWrapper(fmt.Errorf("task_id is empty"), "invalid_response", http.StatusInternalServerError)
	}

	publicBody, err := sanitizeSubmitResponse(responseBody, info.PublicTaskID, info.OriginModelName)
	if err != nil {
		return "", responseBody, service.TaskErrorWrapper(errors.Wrapf(err, "body: %s", responseBody), "unmarshal_response_body_failed", http.StatusInternalServerError)
	}
	service.IOCopyBytesGracefully(c, resp, publicBody)
	return upstreamID, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok || strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("invalid task_id")
	}
	uri := fmt.Sprintf("%s/v1/videos/%s", strings.TrimRight(baseURL, "/"), taskID)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	return parseTaskResultPayload(respBody)
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	video := dto.NewOpenAIVideo()
	video.ID = task.TaskID
	video.TaskID = task.TaskID
	video.Model = task.Properties.OriginModelName
	video.Status = task.Status.ToVideoStatus()
	video.SetProgressStr(task.Progress)
	video.CreatedAt = task.CreatedAt
	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
		video.CompletedAt = task.UpdatedAt
	}
	if url := task.GetResultURL(); url != "" {
		video.SetMetadata("url", url)
	}
	if task.Status == model.TaskStatusFailure {
		video.Error = &dto.OpenAIVideoError{
			Code:    "task_failed",
			Message: task.FailReason,
		}
	}
	return common.Marshal(video)
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func extractUpstreamTaskID(respBody []byte) string {
	payload := map[string]any{}
	if err := common.Unmarshal(respBody, &payload); err != nil {
		return ""
	}
	return firstString(payload, "request_id", "id", "task_id")
}

func sanitizeSubmitResponse(respBody []byte, publicTaskID string, modelName string) ([]byte, error) {
	payload := map[string]any{}
	if err := common.Unmarshal(respBody, &payload); err != nil {
		return nil, err
	}
	payload["id"] = publicTaskID
	payload["task_id"] = publicTaskID
	payload["request_id"] = publicTaskID
	if modelName != "" {
		payload["model"] = modelName
	}
	if _, ok := payload["object"]; !ok {
		payload["object"] = "video"
	}
	if _, ok := payload["status"]; !ok {
		payload["status"] = dto.VideoStatusQueued
	}
	return common.Marshal(payload)
}

func parseTaskResultPayload(respBody []byte) (*relaycommon.TaskInfo, error) {
	payload := map[string]any{}
	if err := common.Unmarshal(respBody, &payload); err != nil {
		return nil, errors.Wrap(err, "unmarshal task result failed")
	}

	taskResult := relaycommon.TaskInfo{Code: 0}
	status := strings.ToLower(firstNestedString(payload, []string{"status"}, []string{"data", "status"}, []string{"output", "status"}))
	switch status {
	case "queued", "pending", "submitted", "created":
		taskResult.Status = model.TaskStatusQueued
		taskResult.Progress = taskcommon.ProgressQueued
	case "processing", "in_progress", "running":
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = taskcommon.ProgressInProgress
	case "done", "completed", "succeeded", "success":
		taskResult.Status = model.TaskStatusSuccess
		taskResult.Progress = taskcommon.ProgressComplete
	case "failed", "failure", "error", "cancelled", "canceled":
		taskResult.Status = model.TaskStatusFailure
		taskResult.Reason = extractErrorMessage(payload)
		if taskResult.Reason == "" {
			taskResult.Reason = "task failed"
		}
	default:
		taskResult.Status = model.TaskStatusUnknown
	}

	if progress := extractProgress(payload); progress != "" {
		taskResult.Progress = progress
	}
	if url := extractVideoURL(payload); url != "" {
		taskResult.Url = url
	}
	return &taskResult, nil
}

func extractProgress(payload map[string]any) string {
	for _, path := range [][]string{{"progress"}, {"data", "progress"}, {"output", "progress"}} {
		if value, ok := nestedValue(payload, path...); ok {
			switch v := value.(type) {
			case float64:
				if v > 0 && v <= 1 {
					v *= 100
				}
				return fmt.Sprintf("%d%%", int(v))
			case int:
				return fmt.Sprintf("%d%%", v)
			case string:
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				if strings.HasSuffix(v, "%") {
					return v
				}
				if _, err := strconv.Atoi(v); err == nil {
					return v + "%"
				}
				return v
			}
		}
	}
	return ""
}

func extractVideoURL(payload map[string]any) string {
	for _, path := range [][]string{
		{"video", "url"},
		{"video_url"},
		{"videoUrl"},
		{"url"},
		{"result_url"},
		{"data", "video", "url"},
		{"data", "video_url"},
		{"data", "videoUrl"},
		{"data", "url"},
		{"data", "result_url"},
		{"output", "video", "url"},
		{"output", "video_url"},
		{"output", "videoUrl"},
		{"output", "url"},
		{"output", "result_url"},
	} {
		if value := strings.TrimSpace(firstNestedString(payload, path)); value != "" {
			return value
		}
	}
	return ""
}

func extractErrorMessage(payload map[string]any) string {
	for _, path := range [][]string{
		{"error", "message"},
		{"error"},
		{"message"},
		{"data", "error", "message"},
		{"data", "message"},
		{"output", "error", "message"},
		{"output", "message"},
	} {
		if value := strings.TrimSpace(firstNestedString(payload, path)); value != "" {
			return value
		}
	}
	return ""
}

func firstNestedString(payload map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value, ok := nestedValue(payload, path...); ok {
			if s := stringValue(value); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := stringValue(payload[key]); s != "" {
			return s
		}
	}
	return ""
}

func nestedValue(payload map[string]any, path ...string) (any, bool) {
	var current any = payload
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}

func useOpenAIVideoCreatePath(baseURL string) bool {
	baseURL = strings.ToLower(strings.TrimSpace(baseURL))
	if baseURL == "" {
		return false
	}
	return !strings.Contains(baseURL, "api.x.ai")
}
