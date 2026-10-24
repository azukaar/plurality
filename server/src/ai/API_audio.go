package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/go-audio/wav"

	"github.com/azukaar/plurality/src/utils"
)

func HandleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Define a struct for the transcription request
	type TranscribeRequest struct {
		AudioData      []byte  `json:"audioData"` // Base64 encoded audio data
		Model          string  `json:"model"`     // Optional, defaults to whisper-v3-turbo
		Language       string  `json:"language"`  // Optional language hint
		Temperature    float64 `json:"temperature"`
		ResponseFormat string  `json:"response_format"` // Optional, defaults to text
		VadModel       string  `json:"vad_model"`       // Optional, defaults to silero
	}

	var request TranscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.Error("[HandleTranscribe] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Set default values if not provided
	if request.Model == "" {
		request.Model = "whisper-v3-turbo"
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "text"
	}
	if request.Temperature == 0 {
		request.Temperature = 0.0 // Default temperature
	}
	if request.VadModel == "" {
		request.VadModel = "silero" // Default VAD model
	}

	if !Models.IsAudioModel(request.Model) {
		utils.Error("[HandleTranscribe] Unknown audio model %s", nil, request.Model)
		utils.SendHTTPError(w, "Unknown model", http.StatusBadRequest)
		return
	}

	utils.Log("[HandleTranscribe] Transcribing audio with model %s, length: %d", request.Model, len(request.AudioData))

	// Call the Whisper API (this function would need to be implemented)
	transcription, err := TranscribeAudio(request.AudioData, request.Model, request.Language, request.Temperature, request.ResponseFormat, request.VadModel)
	if err != nil {
		utils.Error("[HandleTranscribe] Error transcribing audio", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// w.Header().Set("Content-Type", "application/json")
	// messageAsBytes, _ := json.Marshal(message)
	w.Write(([]byte)(transcription))
}

func TranscribeAudio(audioData []byte, model string, language string, temperature float64, responseFormat string, vadModel string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	// Route through the litellm proxy. The upstream URL and API key live in
	// litellm_config.yaml under the model's model_info.endpoint_url.
	apiURL := LiteLLMBaseURL + "/v1/audio/transcriptions"

	// create wav buffer for conversaion
	wavBuffer := &utils.WriteSeekerInMem{}

	wavWriter := wav.NewEncoder(wavBuffer, 8000, 16, 1, 1)

	// Write the audio data to the wav buffer
	err := wavWriter.Write(utils.NewAudioIntBufferFromBuffer(audioData))
	if err != nil {
		return "", fmt.Errorf("error writing audio data to wav buffer: %v", err)
	}
	// Close the wav writer to flush the data
	err = wavWriter.Close()
	if err != nil {
		return "", fmt.Errorf("error closing wav writer: %v", err)
	}

	// Write audio data to file for debug
	// err1 := os.WriteFile("audio.wav", wavBuffer.Bytes(), 0644)
	// if err1 != nil {
	// 		return "", fmt.Errorf("error writing audio data to file: %v", err1)
	// }

	// Create a new multipart writer
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add the model parameter
	err = writer.WriteField("model", model)
	if err != nil {
		return "", fmt.Errorf("error adding model field: %v", err)
	}

	// Add other parameters if provided
	if language != "" {
		err = writer.WriteField("language", language)
		if err != nil {
			return "", fmt.Errorf("error adding language field: %v", err)
		}
	}

	if temperature != 0 {
		err = writer.WriteField("temperature", strconv.FormatFloat(temperature, 'f', 2, 64))
		if err != nil {
			return "", fmt.Errorf("error adding temperature field: %v", err)
		}
	}

	if responseFormat != "" {
		err = writer.WriteField("response_format", responseFormat)
		if err != nil {
			return "", fmt.Errorf("error adding response_format field: %v", err)
		}
	}

	if vadModel != "" {
		err = writer.WriteField("vad_model", vadModel)
		if err != nil {
			return "", fmt.Errorf("error adding vad_model field: %v", err)
		}
	}

	// Create a form file field for the audio data
	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("error creating form file: %v", err)
	}

	// Write the audio data to the form file
	_, err = part.Write(wavBuffer.Bytes())
	if err != nil {
		return "", fmt.Errorf("error writing audio data: %v", err)
	}

	// Close the multipart writer
	err = writer.Close()
	if err != nil {
		return "", fmt.Errorf("error closing multipart writer: %v", err)
	}

	// Create the HTTP request
	req, err := http.NewRequest("POST", apiURL, body)
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}

	// Set the content type to the multipart form's content type. No upstream
	// auth header — the litellm proxy injects the provider's API key.
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Send the request
	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	// Read the response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	utils.Debug("[TranscribeAudio] Response: %s", responseBody)

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error: %s", string(responseBody))
	}

	// Parse the response based on the response format
	if responseFormat == "json" {
		var jsonResponse struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(responseBody, &jsonResponse); err != nil {
			return "", fmt.Errorf("error parsing JSON response: %v", err)
		}
		return jsonResponse.Text, nil
	}

	// For text response format, return the body directly
	return string(responseBody), nil
}
