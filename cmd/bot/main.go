package main

import (
	"log"
	"os"
	"sort"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

const token = "TG_BOT_TOKEN"

// ---- Админы (замени на свои user_id) ----
var admins = map[int64]bool{
	123456789: true,
}

// --------- Состояние планирования ---------

// DraftUnit — один «пост» в будущем списке к отправке
type DraftUnit struct {
	Kind       string // "text" | "photo" | "video" | "document" | "audio" | "voice" | "mediagroup"
	Text       string
	FileID     string        // для одиночных медиа
	MediaGroup []interface{} // []InputMedia для альбома
	CreatedAt  time.Time     // для сохранения порядка
	Order      int64         // для стабильной сортировки
}

// Session — буфер для одного администратора
type Session struct {
	Active        bool
	Units         []DraftUnit
	mediaBuf      map[string][]inputMediaWithIndex // MediaGroupID -> элементы альбома
	mediaCaptions map[string]string                // MediaGroupID -> общий caption (вешаем на первый элемент)
	nextOrder     int64
}

type inputMediaWithIndex struct {
	idx   int // порядковый номер внутри группы (чтобы отсортировать, если придёт не по порядку)
	media interface{}
}

// Все сессии: user_id -> Session
var sessions = map[int64]*Session{}

// ------------------------------------------

func main() {
	initEnv()
	bot := initBot()
	bot.Debug = true

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		msg := update.Message
		userID := msg.From.ID
		chatID := msg.Chat.ID
		text := msg.Text

		// Команды
		if text == "/schedule" {
			if !isAdmin(userID) {
				bot.Send(tgbotapi.NewMessage(chatID, "Нет прав."))
				continue
			}
			s := ensureSession(userID)
			s.Active = true
			s.Units = nil
			s.mediaBuf = map[string][]inputMediaWithIndex{}
			s.mediaCaptions = map[string]string{}
			s.nextOrder = 0
			bot.Send(tgbotapi.NewMessage(chatID, "Режим планирования включён. Пришлите сообщения (текст/медиа/альбом). Завершите командой /done"))
			continue
		}

		if text == "/done" {
			if !isAdmin(userID) {
				bot.Send(tgbotapi.NewMessage(chatID, "Нет прав."))
				continue
			}
			s := sessions[userID]
			if s == nil || !s.Active {
				bot.Send(tgbotapi.NewMessage(chatID, "Режим планирования не активен. Используйте /schedule"))
				continue
			}

			// Сначала материализуем незавершённые медиагруппы в Units
			flushAllMediaGroupsIntoUnits(s)

			// Отправляем всё по порядку
			sort.SliceStable(s.Units, func(i, j int) bool {
				if s.Units[i].CreatedAt.Equal(s.Units[j].CreatedAt) {
					return s.Units[i].Order < s.Units[j].Order
				}
				return s.Units[i].CreatedAt.Before(s.Units[j].CreatedAt)
			})

			for _, u := range s.Units {
				switch u.Kind {
				case "text":
					bot.Send(tgbotapi.NewMessage(chatID, u.Text))
				case "photo":
					cfg := tgbotapi.NewPhoto(chatID, tgbotapi.FileID(u.FileID))
					cfg.Caption = u.Text
					bot.Send(cfg)
				case "video":
					cfg := tgbotapi.NewVideo(chatID, tgbotapi.FileID(u.FileID))
					cfg.Caption = u.Text
					bot.Send(cfg)
				case "document":
					cfg := tgbotapi.NewDocument(chatID, tgbotapi.FileID(u.FileID))
					cfg.Caption = u.Text
					bot.Send(cfg)
				case "audio":
					cfg := tgbotapi.NewAudio(chatID, tgbotapi.FileID(u.FileID))
					cfg.Caption = u.Text
					bot.Send(cfg)
				case "voice":
					cfg := tgbotapi.NewVoice(chatID, tgbotapi.FileID(u.FileID))
					bot.Send(cfg)
				case "mediagroup":
					if len(u.MediaGroup) > 0 {
						msg := tgbotapi.NewMediaGroup(chatID, u.MediaGroup)
						bot.Send(msg)
					}
				}
			}

			// Очистка и выключение режима
			s.Active = false
			s.Units = nil
			s.mediaBuf = nil
			s.mediaCaptions = nil
			bot.Send(tgbotapi.NewMessage(chatID, "Готово. Все собранные сообщения отправлены в одном блоке."))
			continue
		}

		// Если не в режиме планирования — просто эхо твой старый код:
		s := sessions[userID]
		if s == nil || !s.Active {
			echoIncoming(bot, msg)
			continue
		}

		// ---- РЕЖИМ ПЛАНИРОВАНИЯ: буферизуем ----
		// Сначала проверим на медиагруппу
		if msg.MediaGroupID != "" {
			handleMediaGroupMessage(s, msg)
			// В режиме планирования ничего не отправляем сразу
			continue
		}

		// Одиночные сообщения
		switch {
		case msg.Text != "":
			s.Units = append(s.Units, DraftUnit{
				Kind:      "text",
				Text:      msg.Text,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		case msg.Photo != nil:
			ph := msg.Photo[len(msg.Photo)-1] // самое большое
			s.Units = append(s.Units, DraftUnit{
				Kind:      "photo",
				FileID:    ph.FileID,
				Text:      msg.Caption,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		case msg.Video != nil:
			s.Units = append(s.Units, DraftUnit{
				Kind:      "video",
				FileID:    msg.Video.FileID,
				Text:      msg.Caption,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		case msg.Document != nil:
			s.Units = append(s.Units, DraftUnit{
				Kind:      "document",
				FileID:    msg.Document.FileID,
				Text:      msg.Caption,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		case msg.Audio != nil:
			s.Units = append(s.Units, DraftUnit{
				Kind:      "audio",
				FileID:    msg.Audio.FileID,
				Text:      msg.Caption,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		case msg.Voice != nil:
			s.Units = append(s.Units, DraftUnit{
				Kind:      "voice",
				FileID:    msg.Voice.FileID,
				CreatedAt: msg.Time(),
				Order:     s.nextOrderAndInc(),
			})

		default:
			// можно сообщить администратору, что формат не поддержан для планирования
			// но молчим, чтобы не спамить
		}
	}
}

// --------- ВСПОМОГАТЕЛЬНОЕ ---------

func (s *Session) nextOrderAndInc() int64 {
	v := s.nextOrder
	s.nextOrder++
	return v
}

func handleMediaGroupMessage(s *Session, msg *tgbotapi.Message) {
	if s.mediaBuf == nil {
		s.mediaBuf = map[string][]inputMediaWithIndex{}
	}
	if s.mediaCaptions == nil {
		s.mediaCaptions = map[string]string{}
	}

	groupID := msg.MediaGroupID

	// caption сохраняем один раз — повесим на первый элемент при сборке
	if msg.Caption != "" && s.mediaCaptions[groupID] == "" {
		s.mediaCaptions[groupID] = msg.Caption
	}

	// Определяем тип элемента альбома и создаём InputMedia
	idx := 0
	if msg.MessageID != 0 {
		// частично стабилизируем порядок
		idx = msg.MessageID
	}

	switch {
	case msg.Photo != nil:
		ph := msg.Photo[len(msg.Photo)-1]
		im := tgbotapi.NewInputMediaPhoto(tgbotapi.FileID(ph.FileID))
		s.mediaBuf[groupID] = append(s.mediaBuf[groupID], inputMediaWithIndex{idx: idx, media: im})

	case msg.Video != nil:
		vi := tgbotapi.NewInputMediaVideo(tgbotapi.FileID(msg.Video.FileID))
		s.mediaBuf[groupID] = append(s.mediaBuf[groupID], inputMediaWithIndex{idx: idx, media: vi})

	default:
		// в медиагруппах Telegram поддерживает фото/видео. Остальное игнорируем.
	}
}

func flushAllMediaGroupsIntoUnits(s *Session) {
	for groupID, items := range s.mediaBuf {
		if len(items) == 0 {
			continue
		}
		// Отсортируем элементы по idx (на случай, если приехали не по порядку)
		sort.SliceStable(items, func(i, j int) bool { return items[i].idx < items[j].idx })

		var media []interface{}
		for i, it := range items {
			switch m := it.media.(type) {
			case tgbotapi.InputMediaPhoto:
				// caption только на первом элементе, если есть
				if i == 0 {
					m.Caption = s.mediaCaptions[groupID]
				}
				media = append(media, m)
			case tgbotapi.InputMediaVideo:
				if i == 0 {
					m.Caption = s.mediaCaptions[groupID]
				}
				media = append(media, m)
			default:
				// safety
			}
		}

		s.Units = append(s.Units, DraftUnit{
			Kind:       "mediagroup",
			MediaGroup: media,
			CreatedAt:  time.Now(),
			Order:      s.nextOrderAndInc(),
		})
	}
	// очистим буферы
	s.mediaBuf = map[string][]inputMediaWithIndex{}
	s.mediaCaptions = map[string]string{}
}

func echoIncoming(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	switch {
	case msg.Text != "":
		bot.Send(tgbotapi.NewMessage(chatID, msg.Text))
	case msg.Photo != nil:
		ph := msg.Photo[len(msg.Photo)-1]
		cfg := tgbotapi.NewPhoto(chatID, tgbotapi.FileID(ph.FileID))
		cfg.Caption = msg.Caption
		bot.Send(cfg)
	case msg.Document != nil:
		cfg := tgbotapi.NewDocument(chatID, tgbotapi.FileID(msg.Document.FileID))
		cfg.Caption = msg.Caption
		bot.Send(cfg)
	case msg.Audio != nil:
		cfg := tgbotapi.NewAudio(chatID, tgbotapi.FileID(msg.Audio.FileID))
		cfg.Caption = msg.Caption
		bot.Send(cfg)
	case msg.Video != nil:
		cfg := tgbotapi.NewVideo(chatID, tgbotapi.FileID(msg.Video.FileID))
		cfg.Caption = msg.Caption
		bot.Send(cfg)
	case msg.Voice != nil:
		cfg := tgbotapi.NewVoice(chatID, tgbotapi.FileID(msg.Voice.FileID))
		bot.Send(cfg)
	default:
		bot.Send(tgbotapi.NewMessage(chatID, "Тип сообщения пока не поддерживается"))
	}
}

func initBot() *tgbotapi.BotAPI {
	bot, err := tgbotapi.NewBotAPI(os.Getenv(token))
	if err != nil {
		log.Fatal(err)
	}
	return bot
}

func initEnv() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
}

func isAdmin(userID int64) bool {
	return admins[userID]
}

func ensureSession(userID int64) *Session {
	s, ok := sessions[userID]
	if !ok {
		s = &Session{
			Active:        false,
			Units:         []DraftUnit{},
			mediaBuf:      map[string][]inputMediaWithIndex{},
			mediaCaptions: map[string]string{},
			nextOrder:     0,
		}
		sessions[userID] = s
	}
	return s
}
