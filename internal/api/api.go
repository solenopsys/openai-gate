package api

import (
	"fmt"
	"sip-webrtc-openai/internal/controllers"
	"sip-webrtc-openai/internal/store"

	"github.com/gin-gonic/gin"
)

type TranscriptMessage struct {
	Time   string `json:"time"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

type ContextAPI struct {
	store    *store.ContextStore
	recorder *controllers.OpusRecorderController
}

func NewContextAPI(store *store.ContextStore,
	recorder *controllers.OpusRecorderController) *ContextAPI {
	return &ContextAPI{
		store:    store,
		recorder: recorder,
	}
}

func (api *ContextAPI) Start(addr string) error {
	r := gin.Default()

	r.POST("/context/:phone", api.setContext)
	r.GET("/context/:phone", api.getContext)
	r.DELETE("/context/:phone", api.deleteContext)
	r.GET("/contexts", api.listContexts)

	// Новые маршруты для записей и транскриптов
	r.GET("/record/:sessionid", api.getRecord)
	r.GET("/transcript/:sessionid", api.getTranscript)
	r.GET("/user/:user", api.getUserSessions)

	return r.Run(addr)
}

func (api *ContextAPI) setContext(c *gin.Context) {
	phone := c.Param("phone")

	var req struct {
		Context string `json:"context"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	if err := api.store.SetContext(phone, req.Context); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"ok": true})
}

func (api *ContextAPI) getContext(c *gin.Context) {
	phone := c.Param("phone")

	context, err := api.store.GetContext(phone)
	if err != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}

	c.JSON(200, gin.H{"context": context})
}

func (api *ContextAPI) deleteContext(c *gin.Context) {
	phone := c.Param("phone")

	if err := api.store.DeleteContext(phone); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"ok": true})
}

func (api *ContextAPI) getUserSessions(c *gin.Context) {
	user := c.Param("user")

	sessions, err := api.store.GetUserSessions(user, "webrtc")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"sessions": sessions})
}

func (api *ContextAPI) listContexts(c *gin.Context) {
	contexts, err := api.store.ListContexts()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, contexts)
}

func (api *ContextAPI) GetContextByPhone(phone string) (string, error) {
	return api.store.GetContext(phone)
}

// getRecord отдает WebM файл записи сессии
func (api *ContextAPI) getRecord(c *gin.Context) {
	sessionID := c.Param("sessionid")
	source := c.Query("source")

	// Проверяем, что source указан
	if source == "" {
		c.JSON(400, gin.H{"error": "source parameter is required (incoming/outgoing)"})
		return
	}

	// Получаем WebM данные
	data, err := api.recorder.CreateWebMData(sessionID, source)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	// Устанавливаем заголовки для WebM файла
	c.Header("Content-Type", "audio/webm")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_%s.webm\"", sessionID, source))

	// Отправляем данные
	c.Data(200, "audio/webm", data)
}

// getTranscript отдает JSON файл транскрипта сессии
func (api *ContextAPI) getTranscript(c *gin.Context) {
	sessionID := c.Param("sessionid")

	phrases, err := api.store.GetDialogTexts(sessionID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	messages := make([]TranscriptMessage, len(phrases))
	for i, phrase := range phrases {
		messages[i] = TranscriptMessage{
			Time:   fmt.Sprintf("%d", phrase.Time),
			Source: phrase.Source,
			Text:   phrase.Phrase,
		}
	}

	// Отдаем JSON
	c.JSON(200, gin.H{"transcript": messages})
}
