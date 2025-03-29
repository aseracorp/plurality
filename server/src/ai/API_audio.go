package ai

import (
	"encoding/json"
	"net/http"
	"os"
	"io"
	"strconv"
	"fmt"
	"bytes"
	"mime/multipart"

	"github.com/go-audio/wav"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)


func HandleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Define a struct for the transcription request
	type TranscribeRequest struct {
		AudioData      []byte             `json:"audioData"` // Base64 encoded audio data
		Model          string             `json:"model"`     // Optional, defaults to whisper-v3-turbo
		Language       string             `json:"language"`  // Optional language hint
		Temperature    float64            `json:"temperature"`
		ResponseFormat string             `json:"response_format"` // Optional, defaults to text
		VadModel 			 string             `json:"vad_model"`       // Optional, defaults to silero
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

	// Check plan permissions
	planName, err := db.GetPlanName(r.Context())
	if err != nil {
		utils.Error("[HandleTranscribe] Error getting plan name", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !CheckModel(request.Model, planName) {
		utils.Error("[HandleTranscribe] Invalid model %s", nil, request.Model)
		utils.SendHTTPError(w, "Invalid model", http.StatusBadRequest)
		return
	}

	// Calculate price based on audio length (this would need to be implemented)
	// For now, using a placeholder fixed price
	priceToken := 0.1 // This should be replaced with actual pricing logic

	canPerform, err := db.CheckSufficientCredits(r.Context(), priceToken)
	if err != nil {
		utils.Error("[HandleTranscribe] Error checking credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !canPerform {
		utils.Error("[HandleTranscribe] Insufficient credits", err)
		utils.SendHTTPError(w, "Insufficient credits", http.StatusPaymentRequired)
		return
	}

	utils.Log("[HandleTranscribe] Transcribing audio with model %s for %f credits", request.Model, priceToken)

	// Remove credits before processing
	_, err = db.RemoveCredits(r.Context(), priceToken, utils.UserAction{
		Type:     TRANSCRIBE,
		Provider: TOGETHER,
		Model: utils.Model{
			Name: request.Model,
			Params: map[string]string{
				"language":        request.Language,
				"temperature":     strconv.FormatFloat(request.Temperature, 'f', 2, 64),
				"response_format": request.ResponseFormat,
				"vad_model":       request.VadModel,
			},
		},
	})

	if err != nil {
		utils.Error("[HandleTranscribe] Error removing credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	utils.Log("[HandleTranscribe] Request audio data length: %d", len(request.AudioData))

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
	// Define the API endpoint
	apiURL := "https://audio-turbo.us-virginia-1.direct.fireworks.ai/v1/audio/transcriptions"

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
	err = os.WriteFile("audio.wav", wavBuffer.Bytes(), 0644)
	if err != nil {
			return "", fmt.Errorf("error writing audio data to file: %v", err)
	}
	
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

	// Set the content type to the multipart form's content type
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+os.Getenv("FIREWORK_KEY"))

	// Send the request
	client := &http.Client{}
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
