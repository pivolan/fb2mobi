package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

const (
	uploadDir   = "./uploads"
	defaultPort = "11477"
)

type FileStorage struct {
	files map[string]string
	mu    sync.RWMutex
}

var storage = &FileStorage{
	files: make(map[string]string),
}

func init() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal(err)
	}
}

func main() {
	port := flag.String("port", defaultPort, "HTTP server port")
	flag.Parse()

	bot, err := tgbotapi.NewBotAPI(os.Getenv("TELEGRAM_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)

	go startHTTPServer(*port)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if update.Message.IsCommand() && update.Message.Command() == "start" {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"Привет! Отправь мне книгу в формате FB2 или TXT, и я конвертирую её в MOBI формат.")
			bot.Send(msg)
			continue
		}

		if update.Message.Document != nil {
			go handleDocument(bot, update)
		}
	}
}

func generateSlug() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:3], nil
}

func handleDocument(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	doc := update.Message.Document

	fileExt := strings.ToLower(filepath.Ext(doc.FileName))
	if fileExt != ".fb2" && fileExt != ".txt" {
		msg := tgbotapi.NewMessage(chatID, "Пожалуйста, отправьте файл в формате FB2 или TXT")
		bot.Send(msg)
		return
	}

	msg := tgbotapi.NewMessage(chatID, "Начинаю конвертацию файла...")
	bot.Send(msg)

	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: doc.FileID})
	if err != nil {
		log.Printf("Ошибка получения файла: %v", err)
		sendErrorMessage(bot, chatID)
		return
	}

	timeStamp := time.Now().Unix()
	inputPath := filepath.Join(uploadDir, fmt.Sprintf("%d_%s", timeStamp, doc.FileName))
	outputPath := filepath.Join(uploadDir, fmt.Sprintf("%d_%s.mobi", timeStamp,
		strings.TrimSuffix(doc.FileName, filepath.Ext(doc.FileName))))

	err = downloadFile(file.Link(bot.Token), inputPath)
	if err != nil {
		log.Printf("Ошибка сохранения файла: %v", err)
		sendErrorMessage(bot, chatID)
		cleanupFiles(inputPath)
		return
	}

	err = convertFile(inputPath, outputPath)
	if err != nil {
		log.Printf("Ошибка конвертации: %v", err)
		sendErrorMessage(bot, chatID)
		cleanupFiles(inputPath, outputPath)
		return
	}

	slug, err := generateSlug()
	if err != nil {
		log.Printf("Ошибка генерации slug: %v", err)
		sendErrorMessage(bot, chatID)
		cleanupFiles(inputPath, outputPath)
		return
	}

	storage.mu.Lock()
	storage.files[slug] = outputPath
	storage.mu.Unlock()

	downloadURL := fmt.Sprintf("http://%s/%s",
		os.Getenv("SERVER_HOST"),
		slug)

	msgText := fmt.Sprintf("Конвертация завершена. Скачать файл можно по ссылке:\n%s", downloadURL)
	msg = tgbotapi.NewMessage(chatID, msgText)
	bot.Send(msg)

	err = sendConvertedFile(bot, chatID, outputPath)
	if err != nil {
		log.Printf("Ошибка отправки файла: %v", err)
		sendErrorMessage(bot, chatID)
	}

	cleanupFiles(inputPath)
}

func startHTTPServer(port string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/")
		if slug == "" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		storage.mu.RLock()
		filePath, exists := storage.files[slug]
		storage.mu.RUnlock()

		if !exists {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		fileName := filepath.Base(filePath)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/x-mobipocket-ebook")

		http.ServeFile(w, r, filePath)
	})

	log.Printf("Starting HTTP server on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func downloadFile(url, filepath string) error {
	cmd := exec.Command("wget", "-O", filepath, url)
	return cmd.Run()
}

func convertFile(inputPath, outputPath string) error {
	cmd := exec.Command("ebook-convert",
		inputPath,
		outputPath,
		"--output-profile", "kindle",
		"--mobi-file-type", "both")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ошибка конвертации: %v, output: %s", err, string(output))
	}
	return nil
}

func sendConvertedFile(bot *tgbotapi.BotAPI, chatID int64, filePath string) error {
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	file := tgbotapi.FileBytes{
		Name:  filepath.Base(filePath),
		Bytes: fileBytes,
	}

	doc := tgbotapi.NewDocument(chatID, file)
	doc.Caption = "Вот ваша книга в формате MOBI"

	_, err = bot.Send(doc)
	return err
}

func sendErrorMessage(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID,
		"Произошла ошибка при обработке файла. Пожалуйста, попробуйте еще раз.")
	bot.Send(msg)
}

func cleanupFiles(files ...string) {
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			log.Printf("Ошибка удаления файла %s: %v", file, err)
		}
	}
}
