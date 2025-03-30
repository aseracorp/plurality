package ai

import (
	"encoding/json"
	"net/http"
	"os"
	"io"
	"fmt"
	"bytes"
	"time"
	"context"
	"bufio"
	"strings"
	// "encoding/base64"
	
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

func HandleGenerateAudio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Define a struct for the TTS request
	type GenerateAudioRequest struct {
		Input          string  `json:"input"`          // Text to convert to speech
		Model          string  `json:"model"`          // Optional, defaults to "cartesia/sonic"
		Voice          string  `json:"voice"`          // Voice model to use
		ResponseFormat string  `json:"responseFormat"` // Optional, defaults to "wav"
		Speed          string `json:"speed"`          // Optional, speech speed multiplier
	}

	var request GenerateAudioRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.Error("[HandleGenerateAudio] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Set default values if not provided
	if request.Model == "" {
		request.Model = "cartesia/sonic"
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "wav"
	}
	if request.Speed == "" {
		request.Speed = "1.0" // Default speed
	}

	// Check plan permissions
	planName, err := db.GetPlanName(r.Context())
	if err != nil {
		utils.Error("[HandleGenerateAudio] Error getting plan name", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !CheckModel(request.Model, planName) {
		utils.Error("[HandleGenerateAudio] Invalid model %s", nil, request.Model)
		utils.SendHTTPError(w, "Invalid model", http.StatusBadRequest)
		return
	}

	// Calculate price based on input text length
	// This is a placeholder - adjust pricing logic as needed
	inputLength := len(request.Input)
	priceToken := float64(inputLength) * 65.00 / 1000000.00 // Example pricing
	if priceToken < 0.01 {
		priceToken = 0.01 // Minimum price
	}

	canPerform, err := db.CheckSufficientCredits(r.Context(), priceToken)
	if err != nil {
		utils.Error("[HandleGenerateAudio] Error checking credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !canPerform {
		utils.Error("[HandleGenerateAudio] Insufficient credits", nil)
		utils.SendHTTPError(w, "Insufficient credits", http.StatusPaymentRequired)
		return
	}

	utils.Log("[HandleGenerateAudio] Generating audio with model %s for %f credits", request.Model, priceToken)

	// Remove credits before processing
	_, err = db.RemoveCredits(r.Context(), priceToken, utils.UserAction{
		Type:     TTS,
		Provider: TOGETHER,
		Model: utils.Model{
			Name: request.Model,
			Params: map[string]string{
				"voice":           request.Voice,
				"response_format": request.ResponseFormat,
				"speed":           request.Speed,
			},
		},
	})

	if err != nil {
		utils.Error("[HandleGenerateAudio] Error removing credits", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set up streaming response
	// Determine content type based on response format
	contentType := "audio/wav"
	if request.ResponseFormat == "mp3" {
		contentType = "audio/mpeg"
	} else if request.ResponseFormat == "pcm" {
		contentType = "audio/pcm"
	} else if request.ResponseFormat == "opus" {
		contentType = "audio/opus"
	}

	// Set headers for streaming
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")

	// Create a streaming context
	ctx := r.Context()
	
	// Stream the audio generation
	err = StreamGenerateAudio(ctx, w, request.Input, request.Model, request.Voice, request.ResponseFormat, request.Speed)
	if err != nil {
		// If an error occurs during streaming, we can't send a proper error response
		// since we've already started sending the response
		utils.Error("[HandleGenerateAudio] Error streaming audio generation", err)
		// We can try to close the connection to signal an error
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
}

func StreamGenerateAudio(ctx context.Context, w http.ResponseWriter, input string, model string, voice string, responseFormat string, speed string) error {
	// Define the API endpoint
	apiURL := "https://api.together.ai/v1/audio/generations"

	// Create the request body
	requestBody := map[string]interface{}{
		"input":           input,
		"model":           model,
		"voice":           voice,
		"response_format": "raw",
		"stream":          true,
		"sample_rate":     44100,
	}

	// Convert request body to JSON
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("error marshaling request body: %v", err)
	}

	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("TOGETHER_API_KEY"))

	// Create a client with timeout
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	// Send the request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(responseBody))
	}

	// Create a debug file for debugging
	debugFile, err := os.Create("debug.wav")
	if err != nil {
		return fmt.Errorf("error creating debug file: %v", err)
	}
	defer debugFile.Close()

	// Define a struct for SSE JSON response
	type AudioChunk struct {
		Model  string `json:"model"`
		B64    string `json:"b64"`
		Object string `json:"object"`
	}

	// Use a scanner to read the SSE events line by line
	scanner := bufio.NewScanner(resp.Body)
	
	// Buffer for accumulating audio data
	// var audioBuffer bytes.Buffer
	
	for scanner.Scan() {
		line := scanner.Text()
		
		// Skip empty lines
		if line == "" {
			continue
		}
		
		// SSE messages start with "data:"
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		
		// Extract the JSON part
		jsonData := strings.TrimPrefix(line, "data:")
		
		// Parse the JSON
		var chunk AudioChunk
		if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
			return fmt.Errorf("error parsing JSON chunk: %v", err)
		}
		
		// Decode the Base64 data
		// audioData, err := base64.StdEncoding.DecodeString(chunk.B64)
		// if err != nil {
		// 	return fmt.Errorf("error decoding base64 data: %v", err)
		// }
		
		// Write to the response and debug file
		_, writeErr := w.Write(([]byte)(chunk.B64))
		if writeErr != nil {
			return fmt.Errorf("error writing to response: %v", writeErr)
		}
		
		_, writeErr = debugFile.Write(([]byte)(chunk.B64))
		if writeErr != nil {
			return fmt.Errorf("error writing to debug file: %v", writeErr)
		}
		
		// Flush to ensure streaming
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		
		// Check if context is done
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue processing
		}
	}
	
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading SSE stream: %v", err)
	}

	return nil
}
