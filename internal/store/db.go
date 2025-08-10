package store

import (
	"fmt"
	"os"

	"github.com/erigontech/mdbx-go/mdbx"
)

// Database - обертка вокруг MDBX для простых операций
type Database struct {
	env *mdbx.Env
	dbi mdbx.DBI
}

// NewDatabase создает новое подключение к базе данных MDBX
func NewDatabase(dbPath string) (*Database, error) {
	// Создаем окружение MDBX
	env, err := mdbx.NewEnv(mdbx.Label("database"))
	if err != nil {
		return nil, fmt.Errorf("failed to create environment: %w", err)
	}

	// Настраиваем окружение
	if err = env.SetOption(mdbx.OptMaxDB, 1); err != nil {
		env.Close()
		return nil, fmt.Errorf("failed to set max databases: %w", err)
	}

	// Устанавливаем размеры БД (100MB сейчас, макс 1GB)
	if err = env.SetGeometry(-1, 100*1024*1024, 1*1024*1024*1024, -1, -1, -1); err != nil {
		env.Close()
		return nil, fmt.Errorf("failed to set geometry: %w", err)
	}

	// Создаем директорию если не существует
	if err = os.MkdirAll(dbPath, 0755); err != nil {
		env.Close()
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Открываем окружение
	if err = env.Open(dbPath, mdbx.Create, 0664); err != nil {
		env.Close()
		return nil, fmt.Errorf("failed to open environment: %w", err)
	}

	// Открываем базу данных
	var dbi mdbx.DBI
	err = env.Update(func(txn *mdbx.Txn) error {
		dbi, err = txn.CreateDBI("")
		return err
	})
	if err != nil {
		env.Close()
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	return &Database{
		env: env,
		dbi: dbi,
	}, nil
}

// Set сохраняет значение по ключу
func (db *Database) Set(key string, value []byte) error {
	return db.env.Update(func(txn *mdbx.Txn) error {
		return txn.Put(db.dbi, []byte(key), value, 0)
	})
}

// Get получает значение по ключу
func (db *Database) Get(key string) ([]byte, error) {
	var result []byte
	err := db.env.View(func(txn *mdbx.Txn) error {
		val, err := txn.Get(db.dbi, []byte(key))
		if err != nil {
			return err
		}
		// Копируем данные, так как они действительны только внутри транзакции
		result = make([]byte, len(val))
		copy(result, val)
		return nil
	})

	if err != nil && mdbx.IsNotFound(err) {
		return nil, fmt.Errorf("key not found")
	}

	return result, err
}

// Delete удаляет запись по ключу
func (db *Database) Delete(key string) error {
	return db.env.Update(func(txn *mdbx.Txn) error {
		err := txn.Del(db.dbi, []byte(key), nil)
		if err != nil && mdbx.IsNotFound(err) {
			// Игнорируем ошибку "не найден" при удалении
			return nil
		}
		return err
	})
}

// Filter возвращает все записи с указанным префиксом
// Внутри формирует диапазон prefix: - prefix;
func (db *Database) Filter(prefix string) (map[string][]byte, error) {
	result := make(map[string][]byte)

	err := db.env.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(db.dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		// Формируем диапазон: "prefix:" - "prefix;"
		startKey := prefix + ":"
		endKey := prefix + ";"

		startBytes := []byte(startKey)

		// Находим первый ключ >= startKey
		key, val, err := cur.Get(startBytes, nil, mdbx.SetRange)
		if err != nil {
			if mdbx.IsNotFound(err) {
				return nil // Нет записей в диапазоне
			}
			return err
		}

		// Обходим все записи в диапазоне
		for {
			keyStr := string(key)

			// Проверяем, что не вышли за границы диапазона
			if keyStr >= endKey {
				break
			}

			// Копируем значение
			valueCopy := make([]byte, len(val))
			copy(valueCopy, val)
			result[keyStr] = valueCopy

			// Переходим к следующей записи
			key, val, err = cur.Get(nil, nil, mdbx.Next)
			if err != nil {
				if mdbx.IsNotFound(err) {
					break
				}
				return err
			}
		}

		return nil
	})

	return result, err
}

// Close закрывает соединение с базой данных
func (db *Database) Close() error {
	if db.env != nil {
		db.env.Close() // ← Не возвращает ошибку
		db.env = nil
		return nil
	}
	return nil
}
