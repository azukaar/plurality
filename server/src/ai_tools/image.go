package ai_tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

const imagePathMaxBytes = 10 * 1024 * 1024 // 10 MB

// LiteLLMBaseURL is the URL of the local litellm proxy. Set by main from
// ai.LiteLLMBaseURL during bootstrap to avoid an import cycle with the ai
// package.
var LiteLLMBaseURL = "http://127.0.0.1:4000"

var ImageGenTool = utils.AITool{
	Name:              "Image Generation",
	Description:       "Generate an image from a text description using AI image generation",
	ToolID:            "generate_image",
	PickerLabel:       "Image Generation",
	PickerDescription: "Generate images from text descriptions",
	PickerDefault:     "on",
	PickerOrder:       50,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "generate_image",
			Description: "Generate or edit an image from a text description. Write a detailed prompt covering style, composition, and subject. To edit an existing image, pass its attachment ID in the 'attachment' parameter.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"prompt": {
						Type:        "string",
						Description: "Detailed image generation prompt",
					},
					"attachment": {
						Type:        "string",
						Description: "Optional attachment ID from the conversation (e.g. 'att_0') to edit. This is an ID, NOT a file path.",
					},
					"size": {
						Type:        "string",
						Description: "Optional output size as 'WIDTHxHEIGHT' (e.g. '1024x1024' square, '1024x1792' portrait, '1792x1024' landscape). Defaults to 1024x768. Ignored when editing an existing image, where the source aspect ratio is preserved.",
					},
				},
				Required: []string{"prompt"},
			},
		},
	},
	LoadingString: "Generating image: \"{{prompt}}\"",
	IconURL:       "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",
	Exec: func(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
		// Parse arguments
		var params map[string]string
		err := json.Unmarshal([]byte(args), &params)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing arguments: %s", err.Error()))
		}

		prompt := params["prompt"]
		if prompt == "" {
			return utils.NewTextContent("No prompt provided")
		}

		model := params["model"]

		// Resolve input image (either attachment ID or server-side path).
		attachmentID := params["attachment"]
		imagePath := params["path"]
		var inputImageURI string
		width, height := 1024, 768

		if attachmentID != "" && imagePath != "" {
			return utils.NewTextContent("Error: provide only one of 'attachment' or 'path', not both")
		}

		if imagePath != "" {
			if params["_fs_read_enabled"] != "true" {
				return utils.NewTextContent("Error: 'path' requires the file read tool to be enabled")
			}
			uri, ratio, resolveErr := loadImageFromPath(imagePath)
			if resolveErr != nil {
				return utils.NewTextContent(fmt.Sprintf("Error reading image from path: %s", resolveErr.Error()))
			}
			inputImageURI = uri
			width, height = computeOutputDims(ratio)
		} else if attachmentID != "" {
			uri, ratio, resolveErr := ResolveAttachmentImage(attachmentID, conv)
			if resolveErr != nil {
				return utils.NewTextContent(fmt.Sprintf("Error resolving image attachment: %s", resolveErr.Error()))
			}
			inputImageURI = uri
			width, height = computeOutputDims(ratio)
		}

		// Determine output size. For a pure generation the model may request a
		// size; for edits the dimensions follow the source image (computed above).
		sizeStr := fmt.Sprintf("%dx%d", width, height)
		if inputImageURI == "" {
			if s := strings.TrimSpace(params["size"]); s != "" {
				sizeStr = s
			}
		}

		// Generation and editing are different endpoints (mirroring OpenAI's
		// /v1/images/generations vs /v1/images/edits). The model was already
		// swapped to the edit model upstream (tool_loop.go) when an input image
		// is present; here we just pick the matching endpoint and request shape.
		// `size` is the OpenAI-standard param — every litellm provider config
		// translates it to its native shape (fal -> image_size, bedrock ->
		// width/height, vertex -> aspect_ratio, ...).
		var req *http.Request
		if inputImageURI != "" {
			// Edit: multipart POST carrying the input image bytes.
			imgBytes, imgMime, decErr := decodeDataURI(inputImageURI)
			if decErr != nil {
				return utils.NewTextContent(fmt.Sprintf("Error decoding input image: %s", decErr.Error()))
			}
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, fErr := mw.CreateFormFile("image", "image"+extForImageMime(imgMime))
			if fErr != nil {
				return utils.NewTextContent(fmt.Sprintf("Error building edit request: %s", fErr.Error()))
			}
			fw.Write(imgBytes)
			mw.WriteField("model", model)
			mw.WriteField("prompt", prompt)
			mw.WriteField("size", sizeStr)
			mw.WriteField("n", "1")
			mw.WriteField("response_format", "b64_json")
			mw.Close()

			req, err = http.NewRequestWithContext(ctx, "POST", LiteLLMBaseURL+"/v1/images/edits", &buf)
			if err != nil {
				return utils.NewTextContent(fmt.Sprintf("Error creating request: %s", err.Error()))
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
		} else {
			// Generate: JSON POST.
			requestBody := map[string]interface{}{
				"model":           model,
				"prompt":          prompt,
				"size":            sizeStr,
				"n":               1,
				"response_format": "b64_json",
			}
			jsonData, mErr := json.Marshal(requestBody)
			if mErr != nil {
				return utils.NewTextContent(fmt.Sprintf("Error marshaling request: %s", mErr.Error()))
			}
			req, err = http.NewRequestWithContext(ctx, "POST", LiteLLMBaseURL+"/v1/images/generations", bytes.NewBuffer(jsonData))
			if err != nil {
				return utils.NewTextContent(fmt.Sprintf("Error creating request: %s", err.Error()))
			}
			req.Header.Set("Content-Type", "application/json")
		}

		client := utils.HTTPClient
		resp, err := client.Do(req)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error making request: %s", err.Error()))
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading response: %s", err.Error()))
		}

		if resp.StatusCode != http.StatusOK {
			return utils.NewTextContent(fmt.Sprintf("Image request failed with status %d: %s", resp.StatusCode, string(body)))
		}

		// Parse response
		var jsonResponse struct {
			Data []struct {
				B64Json string `json:"b64_json"`
				URL     string `json:"url"`
				Timings struct {
					Inference float64 `json:"inference"`
				} `json:"timings"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &jsonResponse); err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing response: %s", err.Error()))
		}

		if len(jsonResponse.Data) == 0 {
			return utils.NewTextContent("No image data received")
		}

		imageData := jsonResponse.Data[0].B64Json
		infTime := jsonResponse.Data[0].Timings.Inference
		timingText := "Generated in " + strconv.FormatFloat(infTime, 'f', 2, 64) + "s"

		// Some providers return a hosted URL instead of base64; reference it directly.
		if imageData == "" {
			if url := jsonResponse.Data[0].URL; url != "" {
				return utils.NewPartsContent([]utils.ContentPart{
					{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: url}},
					{Type: "text", Text: timingText},
				})
			}
			return utils.NewTextContent("No image data received")
		}

		// Decode the FULL payload and verify it is a real, non-empty image.
		// Some providers return HTTP 200 with a stub body (empty base64,
		// zero inference time, or a placeholder) — observed as the
		// "Generated in 0.00s" dead-end on the face-swap workflow, where the
		// LLM then hallucinated attachment IDs for images that were never
		// persisted ("Attachments not found: att_8"). Fail loudly with a
		// recoverable error so the model can retry / switch model instead of
		// building its next step on a phantom image.
		decodedBytes, decErr := base64.StdEncoding.DecodeString(imageData)
		if decErr != nil || len(decodedBytes) == 0 {
			return utils.NewTextContent(fmt.Sprintf(
				"Image request returned an empty result (no image bytes; inference %.2fs). The provider/model may not support this prompt or was rate-limited. Retry once, or switch to a different image model and try again.",
				infTime))
		}
		if sniffImageMime(decodedBytes) == "" {
			return utils.NewTextContent("Image request returned an unrecognized payload (not a valid PNG/JPEG/WebP image). Retry or switch image model.")
		}

		// Detect actual image format from the base64 data
		mimeType := "image/png"
		if len(imageData) >= 16 {
			if decoded, err := base64.StdEncoding.DecodeString(imageData[:16]); err == nil {
				if len(decoded) >= 3 && decoded[0] == 0xFF && decoded[1] == 0xD8 && decoded[2] == 0xFF {
					mimeType = "image/jpeg"
				} else if len(decoded) >= 4 && string(decoded[:4]) == "\x89PNG" {
					mimeType = "image/png"
				} else if len(decoded) >= 4 && string(decoded[:4]) == "RIFF" {
					mimeType = "image/webp"
				}
			}
		}

		return utils.NewPartsContent([]utils.ContentPart{
			{
				Type:     "image_url",
				ImageURL: &utils.ContentImageURL{URL: "data:" + mimeType + ";base64," + imageData},
			},
			{
				Type: "text",
				Text: timingText,
			},
		})
	},
}

// computeOutputDims preserves the source aspect ratio while keeping total
// pixels around 1024x768, rounded to multiples of 16 and clamped to [256, 2048].
func computeOutputDims(ratio float64) (int, int) {
	targetPixels := 1024.0 * 768.0
	h := int(math.Sqrt(targetPixels / ratio))
	w := int(float64(h) * ratio)
	w = (w + 8) / 16 * 16
	h = (h + 8) / 16 * 16
	if w < 256 {
		w = 256
	}
	if w > 2048 {
		w = 2048
	}
	if h < 256 {
		h = 256
	}
	if h > 2048 {
		h = 2048
	}
	return w, h
}

// loadImageFromPath reads a server-side image file and returns it as a data
// URI plus its aspect ratio (width / height). Reuses resolveServerPath from
// filesystem_server.go for '~'/relative path handling.
func loadImageFromPath(p string) (string, float64, error) {
	resolved, errMsg := resolveServerPath(p)
	if errMsg != "" {
		return "", 0, fmt.Errorf("%s", errMsg)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, err
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("path is a directory: %s", resolved)
	}
	if info.Size() > imagePathMaxBytes {
		return "", 0, fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), imagePathMaxBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", 0, err
	}
	mimeType := sniffImageMime(data)
	if mimeType == "" {
		return "", 0, fmt.Errorf("file does not appear to be a supported image (PNG/JPEG/WebP/GIF)")
	}
	aspect := 4.0 / 3.0
	if cfg, _, decErr := image.DecodeConfig(bytes.NewReader(data)); decErr == nil && cfg.Width > 0 && cfg.Height > 0 {
		aspect = float64(cfg.Width) / float64(cfg.Height)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), aspect, nil
}

func sniffImageMime(b []byte) string {
	if len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF {
		return "image/jpeg"
	}
	if len(b) >= 4 && string(b[:4]) == "\x89PNG" {
		return "image/png"
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a") {
		return "image/gif"
	}
	return ""
}

// decodeDataURI parses a "data:<mime>;base64,<payload>" URI (as produced by
// ResolveAttachmentImage / loadImageFromPath) into raw bytes plus the mime type.
func decodeDataURI(uri string) ([]byte, string, error) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, "", fmt.Errorf("not a data URI")
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("malformed data URI")
	}
	meta := uri[len("data:"):comma] // e.g. "image/png;base64"
	mimeType := "image/png"
	if semi := strings.IndexByte(meta, ';'); semi >= 0 {
		if meta[:semi] != "" {
			mimeType = meta[:semi]
		}
	} else if meta != "" {
		mimeType = meta
	}
	data, err := base64.StdEncoding.DecodeString(uri[comma+1:])
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

// extForImageMime returns a file extension for a multipart filename so the
// upstream provider can sniff the format.
func extForImageMime(m string) string {
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
