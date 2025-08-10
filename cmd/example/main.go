package main

import (
	"fmt"
	"log"
	"os"

	"github.com/erigontech/mdbx-go/mdbx"
)

func main() {
	// Создаем или открываем базу данных
	dbPath := "./testdb"

	// Убираем старую базу для чистого запуска
	os.RemoveAll(dbPath)

	env, err := mdbx.NewEnv(mdbx.Label("example"))
	if err != nil {
		log.Fatal("Ошибка создания окружения:", err)
	}
	defer env.Close()

	// Настраиваем окружение
	err = env.SetOption(mdbx.OptMaxDB, 1)
	if err != nil {
		log.Fatal("Ошибка установки максимального количества БД:", err)
	}

	// Устанавливаем размеры БД (sizeLower, sizeNow, sizeUpper, growthStep, shrinkThreshold, pageSize)
	err = env.SetGeometry(-1, 100*1024*1024, 1*1024*1024*1024, -1, -1, -1) // 100MB сейчас, макс 1GB
	if err != nil {
		log.Fatal("Ошибка установки геометрии:", err)
	}

	// Открываем окружение
	err = env.Open(dbPath, mdbx.Create, 0664)
	if err != nil {
		log.Fatal("Ошибка открытия окружения:", err)
	}

	// Открываем базу данных
	var dbi mdbx.DBI
	err = env.Update(func(txn *mdbx.Txn) error {
		var err error
		dbi, err = txn.CreateDBI("")
		return err
	})
	if err != nil {
		log.Fatal("Ошибка открытия базы данных:", err)
	}

	fmt.Println("=== Пример работы с MDBX-Go ===\n")

	// 1. ВСТАВКА ДАННЫХ
	fmt.Println("1. Вставляем данные:")
	err = env.Update(func(txn *mdbx.Txn) error {
		// Вставляем несколько записей
		data := map[string]string{
			"user:001":    "Алексей",
			"user:002":    "Мария",
			"user:003":    "Петр",
			"user:005":    "Анна",
			"user:010":    "Иван",
			"product:001": "Ноутбук",
			"product:002": "Мышь",
			"product:003": "Клавиатура",
		}

		for key, value := range data {
			err := txn.Put(dbi, []byte(key), []byte(value), 0)
			if err != nil {
				return err
			}
			fmt.Printf("  Вставлено: %s = %s\n", key, value)
		}
		return nil
	})
	if err != nil {
		log.Fatal("Ошибка вставки данных:", err)
	}

	// 2. ПОЛУЧЕНИЕ ДАННЫХ ПО КЛЮЧУ
	fmt.Println("\n2. Получаем данные по ключу:")
	err = env.View(func(txn *mdbx.Txn) error {
		keys := []string{"user:001", "user:003", "product:002", "nonexistent"}

		for _, key := range keys {
			val, err := txn.Get(dbi, []byte(key))
			if err != nil {
				if mdbx.IsNotFound(err) {
					fmt.Printf("  Ключ '%s' не найден\n", key)
				} else {
					return err
				}
			} else {
				fmt.Printf("  %s = %s\n", key, string(val))
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal("Ошибка получения данных:", err)
	}

	// 3. ОБХОД СПИСКА С ОПРЕДЕЛЕННОГО ЗНАЧЕНИЯ
	fmt.Println("\n3. Обходим записи начиная с 'product:':")
	err = env.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		// Начинаем с определенного ключа
		startKey := []byte("product:")

		// Устанавливаем курсор на первый ключ >= startKey с помощью SetOp
		key, val, err := cur.Get(startKey, nil, mdbx.SetRange)
		if err != nil {
			if mdbx.IsNotFound(err) {
				fmt.Println("  Записи не найдены")
				return nil
			}
			return err
		}

		fmt.Println("  Найденные записи:")
		for {
			// Проверяем, что ключ все еще начинается с префикса
			if len(key) < len(startKey) || string(key[:len(startKey)]) != string(startKey) {
				break
			}

			fmt.Printf("    %s = %s\n", string(key), string(val))

			// Переходим к следующей записи
			key, val, err = cur.Get(nil, nil, mdbx.Next)
			if err != nil {
				if mdbx.IsNotFound(err) {
					break // Достигли конца
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal("Ошибка обхода данных:", err)
	}

	// 4. ОБХОД ВСЕХ ЗАПИСЕЙ ПОЛЬЗОВАТЕЛЕЙ
	fmt.Println("\n4. Обходим всех пользователей начиная с 'user:003':")
	err = env.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		// Начинаем с user:003
		startKey := []byte("user:003")
		prefix := []byte("user:")

		key, val, err := cur.Get(startKey, nil, mdbx.SetRange)
		if err != nil {
			if mdbx.IsNotFound(err) {
				fmt.Println("  Записи не найдены")
				return nil
			}
			return err
		}

		fmt.Println("  Пользователи начиная с user:003:")
		for {
			// Проверяем префикс
			if len(key) < len(prefix) || string(key[:len(prefix)]) != string(prefix) {
				break
			}

			fmt.Printf("    %s = %s\n", string(key), string(val))

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
	if err != nil {
		log.Fatal("Ошибка обхода пользователей:", err)
	}

	// 5. ОБХОД ВСЕХ ЗАПИСЕЙ В БАЗЕ
	fmt.Println("\n5. Все записи в базе данных:")
	err = env.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		// Начинаем с первой записи
		key, val, err := cur.Get(nil, nil, mdbx.First)
		if err != nil {
			if mdbx.IsNotFound(err) {
				fmt.Println("  База данных пуста")
				return nil
			}
			return err
		}

		fmt.Println("  Все записи:")
		for {
			fmt.Printf("    %s = %s\n", string(key), string(val))

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
	if err != nil {
		log.Fatal("Ошибка обхода всех записей:", err)
	}

	// 6. СТАТИСТИКА БАЗЫ ДАННЫХ
	fmt.Println("\n6. Информация об окружении:")
	err = env.View(func(txn *mdbx.Txn) error {
		info, err := env.Info(txn)
		if err != nil {
			return err
		}

		fmt.Printf("  Размер карты: %d байт\n", info.MapSize)
		fmt.Printf("  ID последней транзакции: %d\n", info.LastTxnID)
		fmt.Printf("  Размер страницы: %d байт\n", info.PageSize)

		return nil
	})
	if err != nil {
		log.Fatal("Ошибка получения информации:", err)
	}

	stat, err := env.Stat()
	if err != nil {
		log.Fatal("Ошибка получения статистики:", err)
	}

	fmt.Printf("  Глубина дерева: %d\n", stat.Depth)
	fmt.Printf("  Количество страниц-ветвей: %d\n", stat.BranchPages)
	fmt.Printf("  Количество страниц-листьев: %d\n", stat.LeafPages)
	fmt.Printf("  Количество записей: %d\n", stat.Entries)

	fmt.Println("\n=== Пример завершен ===")
}

// Дополнительные вспомогательные функции

// insertBatch - пример пакетной вставки данных
func insertBatch(env *mdbx.Env, dbi mdbx.DBI, prefix string, count int) error {
	return env.Update(func(txn *mdbx.Txn) error {
		for i := 0; i < count; i++ {
			key := fmt.Sprintf("%s%03d", prefix, i)
			value := fmt.Sprintf("Значение %d", i)

			err := txn.Put(dbi, []byte(key), []byte(value), 0)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// countRecordsWithPrefix - подсчитывает количество записей с определенным префиксом
func countRecordsWithPrefix(env *mdbx.Env, dbi mdbx.DBI, prefix string) (int, error) {
	var count int

	err := env.View(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		prefixBytes := []byte(prefix)
		key, _, err := cur.Get(prefixBytes, nil, mdbx.SetRange)
		if err != nil {
			if mdbx.IsNotFound(err) {
				return nil // count останется 0
			}
			return err
		}

		for {
			if len(key) < len(prefixBytes) || string(key[:len(prefixBytes)]) != prefix {
				break
			}

			count++

			key, _, err = cur.Get(nil, nil, mdbx.Next)
			if err != nil {
				if mdbx.IsNotFound(err) {
					break
				}
				return err
			}
		}

		return nil
	})

	return count, err
}

// deleteRecordsWithPrefix - удаляет все записи с определенным префиксом
func deleteRecordsWithPrefix(env *mdbx.Env, dbi mdbx.DBI, prefix string) error {
	return env.Update(func(txn *mdbx.Txn) error {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		prefixBytes := []byte(prefix)
		key, _, err := cur.Get(prefixBytes, nil, mdbx.SetRange)
		if err != nil {
			if mdbx.IsNotFound(err) {
				return nil // Нет записей для удаления
			}
			return err
		}

		for {
			if len(key) < len(prefixBytes) || string(key[:len(prefixBytes)]) != prefix {
				break
			}

			// Удаляем текущую запись
			err = cur.Del(0)
			if err != nil {
				return err
			}

			// Переходим к следующей записи (или остаемся на месте если текущая удалена)
			key, _, err = cur.Get(nil, nil, mdbx.GetCurrent)
			if err != nil {
				if mdbx.IsNotFound(err) {
					break
				}
				// Если текущая позиция недействительна, пробуем следующую
				key, _, err = cur.Get(nil, nil, mdbx.Next)
				if err != nil {
					if mdbx.IsNotFound(err) {
						break
					}
					return err
				}
			}
		}

		return nil
	})
}
