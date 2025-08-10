package store

import (
	"fmt"
	"log"
	"strconv"
	"strings"
)

type ContextStore struct {
	db *Database
}

type OpusBatch struct {
	Session string
	Source  string
	Frames  map[int64][]byte
}

type DialogPhrase struct {
	Time   int64
	Source string
	Phrase string
}

func NewContextStore(dbPath string) (*ContextStore, error) {
	db, err := NewDatabase(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	return &ContextStore{
		db: db,
	}, nil
}

func (s *ContextStore) SaveUserSession(user string, session string, srcType string, time int64) (string, error) {
	key := "user:" + user + ":" + srcType + ":" + fmt.Sprintf("%d", time)
	err := s.db.Set(key, []byte(session))
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *ContextStore) DeleteUserSession(user string, session string, srcType string, time int64) error {
	key := "user:" + user + ":" + srcType + ":" + fmt.Sprintf("%d", time)
	err := s.db.Delete(key)
	if err != nil {
		return err
	}
	return nil
}

// delete opus frames
func (s *ContextStore) DeleteOpusFrames(session string, source string) error {
	prefix := "audio:" + session + ":" + source
	err := s.db.DeleteByPrefix(prefix)
	if err != nil {
		return err
	}
	return nil
}

func (s *ContextStore) GetUserSessions(user string, srcType string) ([]string, error) {
	prefix := "user:" + user + ":" + srcType
	listSessions, err := s.db.Filter(prefix)
	if err != nil {
		return nil, err
	}

	sessions := []string{}
	for _, value := range listSessions {
		sessions = append(sessions, string(value))
	}
	return sessions, nil
}

func (s *ContextStore) SaveOpusFrame(session string, source string, frame []byte, time int64) (string, error) {

	key := "audio:" + session + ":" + source + ":" + fmt.Sprintf("%d", time)
	// logs

	log.Println("SaveOpusFrame", key)
	err := s.db.Set(key, frame)
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *ContextStore) SaveDialogPhrase(session string, source string, phrase string, time int64) (string, error) {
	key := "text:" + session + ":" + source + ":" + fmt.Sprintf("%d", time)
	// logs
	log.Println("SaveDialogPhrase", key)
	err := s.db.Set(key, []byte(phrase))
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *ContextStore) GetDialogTexts(session string) ([]DialogPhrase, error) {
	prefix := "text:" + session
	// logs
	log.Println("GetDialogTexts", prefix)
	listPhrases, err := s.db.Filter(prefix)
	if err != nil {
		return nil, err
	}
	log.Println("GetDialogTexts", listPhrases)
	phrases := []DialogPhrase{}
	for key, value := range listPhrases {

		// split key: text:session:source:time
		parts := strings.Split(key, ":")
		if len(parts) < 4 {
			continue // пропускаем некорректные ключи
		}

		timeString := parts[3]
		time, err := strconv.ParseInt(timeString, 10, 64)
		if err != nil {
			continue
		}

		source := parts[2]
		phrases = append(phrases, DialogPhrase{
			Time:   time,
			Source: source,
			Phrase: string(value),
		})
	}

	return phrases, nil
}

func (s *ContextStore) DeleteDialogTexts(session string) error {
	prefix := "text:" + session
	err := s.db.DeleteByPrefix(prefix)
	if err != nil {
		return err
	}
	return nil
}

func (s *ContextStore) GetOpusFrames(session string, source string) (*OpusBatch, error) {
	prefix := "audio:" + session + ":" + source
	listFrames, err := s.db.Filter(prefix)
	if err != nil {
		return nil, err
	}

	batch := OpusBatch{
		Session: session,
		Source:  source,
		Frames:  make(map[int64][]byte),
	}

	for key, value := range listFrames {
		// split key: audio:sessionId:source:time
		parts := strings.Split(key, ":")
		if len(parts) < 4 {
			continue // пропускаем некорректные ключи
		}

		timeString := parts[3] // время находится в 4-м элементе (индекс 3)
		time, err := strconv.ParseInt(timeString, 10, 64)
		if err != nil {
			continue // пропускаем некорректные записи
		}
		batch.Frames[time] = value
	}

	return &batch, nil
}

func (s *ContextStore) SetContext(phone, context string) error {
	return s.db.Set("context:"+phone, []byte(context))
}

func (s *ContextStore) GetContext(phone string) (string, error) {
	data, err := s.db.Get("context:" + phone)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *ContextStore) DeleteContext(phone string) error {
	return s.db.Delete("context:" + phone)
}

func (s *ContextStore) ListContexts() (map[string]string, error) {
	data, err := s.db.Filter("context")

	if err != nil {
		return nil, err
	}

	contexts := make(map[string]string)
	for key, value := range data {
		if len(key) > 8 {
			phone := key[8:] // убираем "context:"
			contexts[phone] = string(value)
		}
	}

	return contexts, nil
}

func (s *ContextStore) ListSessions() ([]string, error) {
	data, err := s.db.Filter("user")

	log.Println("ListSessions", data)

	if err != nil {
		return nil, err
	}

	// Используем map для уникальных сессий
	uniqueSessions := make(map[string]bool)

	// Извлекаем sessionID из VALUES, а не из ключей!
	for _, value := range data {
		sessionID := string(value)
		if sessionID != "" {
			uniqueSessions[sessionID] = true
		}
	}

	sessions := []string{}
	for sessionID := range uniqueSessions {
		sessions = append(sessions, sessionID)
	}

	return sessions, nil
}

func (s *ContextStore) Close() error {
	return s.db.Close()
}
