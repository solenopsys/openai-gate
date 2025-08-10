package controllers

import (
	"encoding/json"
	"sip-webrtc-openai/internal/store"
	"time"
)

type TranscriptMessage struct {
	Time   string `json:"time"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

type TranscriptController struct {
	store *store.ContextStore
}

func NewTranscriptController(store *store.ContextStore) *TranscriptController {
	return &TranscriptController{store: store}
}

func (tc *TranscriptController) parseOpenAIMessage(rawData []byte) (string, string, bool) {
	var msg map[string]interface{}
	if err := json.Unmarshal(rawData, &msg); err != nil {
		return "", "", false
	}

	msgType, ok := msg["type"].(string)
	if !ok {
		return "", "", false
	}

	var text, source string
	switch msgType {
	case "conversation.item.input_audio_transcription.completed":
		if transcript, exists := msg["transcript"].(string); exists {
			text, source = transcript, UserSource
		}
	case "response.audio_transcript.done":
		if transcript, exists := msg["transcript"].(string); exists {
			text, source = transcript, AiSource
		}
	default:
		return "", "", false
	}

	return source, text, text != ""
}

func (tc *TranscriptController) AddMessage(sessionID string, rawData []byte) error {
	source, text, ok := tc.parseOpenAIMessage(rawData)
	if !ok {
		return nil
	}
	_, err := tc.store.SaveDialogPhrase(sessionID, source, text, time.Now().Unix())
	return err
}
