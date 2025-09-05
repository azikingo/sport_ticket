package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/gen2brain/beeep"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"github.com/lithammer/fuzzysearch/fuzzy"
)

const (
	url             = "https://ticketon.kz/almaty/sports"
	pollInterval    = 50 * time.Second
	seenEventsFile  = "seen_events.json"
	subscribersFile = "subscribers.txt"
	notifyInterval  = 10 * time.Second // Increased to avoid spam
	spamDuration    = 2 * time.Minute  // Reduced spam duration
	httpTimeout     = 30 * time.Second
	maxRetries      = 3
)

type EventData struct {
	Events    map[string]Event `json:"events"`
	LastCheck time.Time        `json:"last_check"`
}

type App struct {
	bot           *tgbotapi.BotAPI
	subscribers   []int64
	subscribersMu sync.RWMutex
	eventData     *EventData
	eventDataMu   sync.RWMutex
	stopNotify    map[int64]chan struct{}
	stopNotifyMu  sync.RWMutex
	httpClient    *http.Client
	ctx           context.Context
	cancel        context.CancelFunc
	telegramToken string
	adminChatID   int64
}

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using system environment variables")
	}

	// Setup logging
	logFile, err := os.OpenFile("checker.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	log.Println("Starting Ticketon checker...")

	// Load configuration from environment
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	adminChatIDStr := os.Getenv("ADMIN_CHAT_ID")
	if adminChatIDStr == "" {
		log.Fatal("ADMIN_CHAT_ID environment variable is required")
	}

	adminChatID, err := strconv.ParseInt(adminChatIDStr, 10, 64)
	if err != nil {
		log.Fatal("Invalid ADMIN_CHAT_ID format:", err)
	}

	// Initialize bot
	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}
	bot.Debug = false

	log.Printf("Bot authorized on account %s", bot.Self.UserName)

	// Create app context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize app
	app := &App{
		bot:           bot,
		stopNotify:    make(map[int64]chan struct{}),
		httpClient:    &http.Client{Timeout: httpTimeout},
		ctx:           ctx,
		cancel:        cancel,
		telegramToken: telegramToken,
		adminChatID:   adminChatID,
		subscribers:   []int64{adminChatID},
	}

	// Load data
	app.loadEventData()
	app.loadSubscribers()

	log.Printf("Loaded %d subscribers and %d events", len(app.subscribers), len(app.eventData.Events))

	// Send startup message to admin
	app.sendMessage(adminChatID, fmt.Sprintf("🚀 Ticketon checker started!\n\nBot: @%s\nSubscribers: %d\nTracked events: %d\n\nUse /help for commands",
		bot.Self.UserName, len(app.subscribers), len(app.eventData.Events)))

	// Setup graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start background routines
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		app.listenForCommands()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		app.pollEvents()
	}()

	log.Println("Bot is running. Press Ctrl+C to stop.")

	// Wait for shutdown signal
	<-sigChan
	log.Println("Shutting down...")
	app.sendMessage(adminChatID, "🛑 Bot is shutting down...")
	cancel()
	wg.Wait()
	log.Println("Shutdown complete")
}

func (app *App) pollEvents() {
	// Check immediately on startup
	app.checkEvents()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-app.ctx.Done():
			return
		case <-ticker.C:
			app.checkEvents()

			// Add random jitter to the next interval to appear more human-like
			jitter := time.Duration(rand.Intn(10)) * time.Second
			ticker.Reset(pollInterval + jitter)
			log.Printf("Next check in %v", pollInterval+jitter)
		}
	}
}

func (app *App) checkEvents() {
	log.Println("Checking for new events...")

	newEvents, err := app.fetchEvents()
	if err != nil {
		log.Printf("Error fetching events: %v", err)
		return
	}

	//newEvents["https://ticketon.kz/almaty/event/fk-kayrat-vs-fk-real-madrid"] = Event{
	//	Title:    "ФК «Кайрат» - ФК «Реал Мадрид»",
	//	Href:     "https://ticketon.kz/almaty/event/fk-kayrat-vs-fk-real-madrid",
	//	DateTime: "вт 30 сен, 21:45",
	//	Price:    "от 15000",
	//	Currency: "KZT",
	//}

	//newEvents["https://ticketon.kz/almaty/event/fk-kayrat-vs-fk-real-madrid"] = Event{
	//	Title:    "FC Kairat - FC Real Madrid»",
	//	Href:     "https://ticketon.kz/almaty/event/fk-kayrat-vs-fk-real-madrid",
	//	DateTime: "вт 30 сен, 21:45",
	//	Price:    "от 15000",
	//	Currency: "KZT",
	//}
	//
	//newEvents["https://ticketon.kz/almaty/event/jony-almaty"] = Event{
	//	Title:    "Jony в Алматы",
	//	Href:     "https://ticketon.kz/almaty/event/jony-almaty",
	//	DateTime: "вс 7 сен, 17:00",
	//	Price:    "от 12000",
	//	Currency: "KZT",
	//}

	app.eventDataMu.Lock()
	existingEvents := len(app.eventData.Events)
	app.eventDataMu.Unlock()

	if existingEvents == 0 {
		// First run - just save events without notifying
		app.eventDataMu.Lock()
		for href, event := range newEvents {
			app.eventData.Events[href] = event
		}
		app.eventData.LastCheck = time.Now()
		app.eventDataMu.Unlock()
		app.saveEventData()
		log.Printf("Initial load: found %d events", len(newEvents))
		return
	}

	// Find truly new events
	var trulyNewEvents []Event
	app.eventDataMu.Lock()
	for href, event := range newEvents {
		if _, exists := app.eventData.Events[href]; !exists {
			trulyNewEvents = append(trulyNewEvents, event)
			app.eventData.Events[href] = event
		}
	}
	app.eventData.LastCheck = time.Now()
	app.eventDataMu.Unlock()

	if len(trulyNewEvents) > 0 {
		log.Printf("Found %d new events", len(trulyNewEvents))
		app.saveEventData()
		app.notifySubscribers(trulyNewEvents)
	} else {
		log.Println("No new events found")
	}
}

// Updated Event struct to capture all available details
type Event struct {
	Title     string `json:"title"`
	Href      string `json:"href"`
	DateTime  string `json:"datetime"` // Combined date and time
	Price     string `json:"price"`
	PosterURL string `json:"poster_url"`
	Currency  string `json:"currency"`
}

// Updated parsing section in your fetchEvents method
func (app *App) parseEvents(doc *goquery.Document) map[string]Event {
	events := make(map[string]Event)

	selectors := []string{
		"a.DetailedCardWrapper_eventItem__ZS8dA",
		".event-item a",
		".event-card a",
		"a[href*='/event/']",
	}

	var foundElements int
	for _, selector := range selectors {
		doc.Find(selector).Each(func(i int, s *goquery.Selection) {
			foundElements++
			href, exists := s.Attr("href")
			if !exists {
				return
			}

			// Extract title from main info section
			title := strings.TrimSpace(s.Find(".DetailedCardInfo_eventTitle__7VjTp").Text())
			if title == "" {
				// Fallback to other selectors
				titleSelectors := []string{".event-title", ".title", "h3", "h2"}
				for _, titleSel := range titleSelectors {
					title = strings.TrimSpace(s.Find(titleSel).Text())
					if title != "" {
						break
					}
				}
			}

			if title == "" {
				return // Skip if no title found
			}

			// Check if tickets are available
			buyBtn := strings.TrimSpace(s.Find(".DetailedCardHover_eventButton__tamEb").Text())
			if !strings.Contains(strings.ToLower(buyBtn), "купить") &&
				!strings.Contains(strings.ToLower(buyBtn), "билет") &&
				!strings.Contains(strings.ToLower(buyBtn), "buy") {
				return // Skip if no tickets available
			}

			// Extract date and time from the info rows
			var dateTime, price string

			s.Find(".DetailedCardInfoItem_eventInfoRow__9douU").Each(func(j int, row *goquery.Selection) {
				iconSVG := row.Find("svg").First()
				text := strings.TrimSpace(row.Find(".DetailedCardInfoItem_eventInfoText__P5XSL").Text())

				if text == "" {
					return
				}

				// Determine what this row contains based on the icon or content
				if iconSVG.Length() > 0 {
					svgHTML, err := iconSVG.Html()
					if err != nil {
						log.Printf("Warning: Failed to get SVG HTML: %v", err)
						return
					}

					// Check for calendar icon (date/time)
					if strings.Contains(svgHTML, "M15.4444 3.55591H4.55556") ||
						strings.Contains(svgHTML, "stroke-width=\"1.5\"") {
						// This looks like a calendar icon - it's date/time
						if strings.Contains(text, ":") { // Has time format
							dateTime = text
						}
					}
				} else {
					// Check for currency icon (price)
					currencyImg := row.Find("img[alt='currencyIcon']")
					if currencyImg.Length() > 0 {
						price = text
					}
				}
			})

			// Extract poster URL
			posterURL := ""
			posterImg := s.Find(".ImageWithSkeleton_content__j1yKX")
			if posterImg.Length() > 0 {
				if src, exists := posterImg.Attr("src"); exists {
					if strings.HasPrefix(src, "/") && !strings.HasPrefix(src, "//") {
						posterURL = "https://ticketon.kz" + src
					} else if strings.HasPrefix(src, "http") {
						posterURL = src
					}
				}
			}

			// Determine currency from price or default
			currency := "KZT" // Default currency
			if strings.Contains(price, "₽") {
				currency = "RUB"
			} else if strings.Contains(price, "$") {
				currency = "USD"
			} else if strings.Contains(price, "€") {
				currency = "EUR"
			}

			// Ensure full URL
			if strings.HasPrefix(href, "/") {
				href = "https://ticketon.kz" + href
			}

			event := Event{
				Title:     title,
				Href:      href,
				DateTime:  dateTime,
				Price:     price,
				PosterURL: posterURL,
				Currency:  currency,
			}

			events[href] = event

			// Log the parsed event for debugging
			log.Printf("Parsed event: %s | %s | %s | %s | %s",
				event.Title, event.DateTime, event.Price)
		})

		if foundElements > 0 {
			break // Found events with this selector
		}
	}

	return events
}

// Updated fetchEvents method with the new parsing
func (app *App) fetchEvents() (map[string]Event, error) {
	var resp *http.Response
	var err error
	var finalURL string

	// Create a custom HTTP client with cookie jar for session persistence
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 60 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				log.Printf("Detected redirect loop after %d redirects. Stopping.", len(via))
				return http.ErrUseLastResponse
			}

			log.Printf("Redirect %d: %s -> %s", len(via), via[len(via)-1].URL.String(), req.URL.String())

			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
			req.Header.Set("Accept-Language", "en-US,en;q=0.5")
			req.Header.Set("Referer", via[len(via)-1].URL.String())

			return nil
		},
	}

	// Retry logic with exponential backoff
	for i := 0; i < maxRetries; i++ {
		req, reqErr := http.NewRequestWithContext(app.ctx, "GET", url, nil)
		if reqErr != nil {
			return nil, fmt.Errorf("failed to create request: %w", reqErr)
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.5")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")

		resp, err = client.Do(req)

		if err == nil {
			finalURL = resp.Request.URL.String()
			log.Printf("Request completed. Final URL: %s, Status: %d", finalURL, resp.StatusCode)

			if resp.StatusCode == 302 || strings.Contains(finalURL, "queue.ticketon.kz") {
				log.Printf("Still in queue system. Status: %d", resp.StatusCode)

				if resp != nil {
					resp.Body.Close()
				}

				if i < maxRetries-1 {
					waitTime := time.Duration(math.Pow(2, float64(i+1))) * 10 * time.Second
					log.Printf("Queue detected. Waiting %v before retry %d/%d", waitTime, i+2, maxRetries)

					select {
					case <-app.ctx.Done():
						return nil, fmt.Errorf("context cancelled during queue wait")
					case <-time.After(waitTime):
					}
				}
				continue
			}

			if resp.StatusCode == 200 {
				break
			}
		}

		if resp != nil {
			log.Printf("Attempt %d failed with status: %d, URL: %s", i+1, resp.StatusCode, finalURL)
			resp.Body.Close()
		} else {
			log.Printf("Attempt %d failed with error: %v", i+1, err)
		}

		if i < maxRetries-1 {
			waitTime := time.Duration(i+1) * 5 * time.Second
			log.Printf("Retry %d/%d after %v", i+1, maxRetries, waitTime)

			select {
			case <-app.ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry wait")
			case <-time.After(waitTime):
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to fetch after %d retries: %w", maxRetries, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d, final URL: %s", resp.StatusCode, finalURL)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	bodyString := string(bodyBytes)

	queueIndicators := []string{
		"queue.ticketon.kz",
		"Please wait while we are checking your browser",
		"Cloudflare",
		"cf-browser-verification",
		"DDoS protection",
		"Checking your browser",
		"This process is automatic",
		"You will be redirected",
		"Ray ID:",
		"Performance & security by Cloudflare",
	}

	for _, indicator := range queueIndicators {
		if strings.Contains(bodyString, indicator) {
			log.Printf("Response still contains queue content, detected: %s", indicator)
			return nil, fmt.Errorf("request still in queue system, response contains: %s", indicator)
		}
	}

	if len(bodyString) < 1000 {
		log.Printf("Response too short (%d chars), might be a redirect page", len(bodyString))
		return nil, fmt.Errorf("response too short, likely a redirect page")
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(bodyString))
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}

	// Check if we actually got the target page
	expectedSelectors := []string{
		"a.DetailedCardWrapper_eventItem__ZS8dA",
		".event-item",
		".event-card",
		"[class*='event']",
		"a[href*='/event/']",
	}

	hasExpectedContent := false
	for _, selector := range expectedSelectors {
		if doc.Find(selector).Length() > 0 {
			hasExpectedContent = true
			break
		}
	}

	if !hasExpectedContent {
		log.Printf("Response doesn't contain expected event content")
		preview := bodyString
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		log.Printf("Response preview: %s", preview)
		return nil, fmt.Errorf("response doesn't contain expected event content")
	}

	events := app.parseEvents(doc)
	log.Printf("Scraped %d events from website", len(events))
	return events, nil
}

// normalize string: lowercase, remove quotes, trim spaces, unify Cyrillic/Latin lookalikes
func normalizeText(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "«", "")
	s = strings.ReplaceAll(s, "»", "")
	s = strings.ReplaceAll(s, "“", "")
	s = strings.ReplaceAll(s, "”", "")
	s = strings.ReplaceAll(s, "фк", "") // remove club prefix
	s = strings.TrimSpace(s)

	// Replace Cyrillic characters with Latin approximations
	replacements := map[rune]rune{
		'а': 'a', 'ә': 'a',
		'б': 'b',
		'в': 'v',
		'г': 'g', 'ғ': 'g',
		'д': 'd',
		'е': 'e', 'ё': 'e',
		'ж': 'j',
		'з': 'z',
		'и': 'i', 'й': 'i',
		'к': 'k', 'қ': 'k',
		'л': 'l',
		'м': 'm',
		'н': 'n', 'ң': 'n',
		'о': 'o', 'ө': 'o',
		'п': 'p',
		'р': 'r',
		'с': 's',
		'т': 't',
		'у': 'u',
		'ұ': 'u', 'ү': 'u',
		'ф': 'f',
		'х': 'h', 'һ': 'h',
		'ц': 'c',
		'ч': 'c',
		'ш': 's', 'щ': 's',
		'ы': 'y', 'і': 'i',
		'э': 'e',
		'ю': 'u',
		'я': 'a',
	}

	var sb strings.Builder
	for _, ch := range s {
		if repl, ok := replacements[ch]; ok {
			sb.WriteRune(repl)
		} else if unicode.IsLetter(ch) || unicode.IsSpace(ch) || ch == '-' {
			sb.WriteRune(ch)
		}
	}
	return sb.String()
}

// check if event title looks like Kairat vs Big team
func isSpecialEvent(title string) bool {
	normalized := normalizeText(title)

	kairatVariants := []string{"qairat", "kairat", "qayrat", "kayrat"}

	// fuzzy check for Kairat
	for _, variant := range kairatVariants {
		if fuzzy.RankMatch(variant, normalized) != -1 {
			return true
		}
	}

	return false
}

// --- Updated notifySubscribers ---
func (app *App) notifySubscribers(events []Event) {
	if len(events) == 0 {
		return
	}

	// Desktop notification
	eventTitles := make([]string, len(events))
	for i, event := range events {
		eventTitles[i] = event.Title
	}
	message := fmt.Sprintf("New sports events: %s", strings.Join(eventTitles, ", "))
	if err := beeep.Alert("Ticketon Alert!", message, ""); err != nil {
		log.Printf("Desktop notification error: %v", err)
	}

	// Telegram notifications
	app.subscribersMu.RLock()
	subscribers := make([]int64, len(app.subscribers))
	copy(subscribers, app.subscribers)
	app.subscribersMu.RUnlock()

	for _, chatID := range subscribers {
		app.stopNotifyMu.Lock()
		if stopChan, exists := app.stopNotify[chatID]; exists {
			close(stopChan)
		}
		stopChan := make(chan struct{})
		app.stopNotify[chatID] = stopChan
		app.stopNotifyMu.Unlock()

		// Decide spam or single send
		if len(events) == 1 && isSpecialEvent(events[0].Title) {
			go app.spamTelegramNotifications(chatID, events, stopChan)
		} else {
			go app.sendTelegramNotifications(chatID, events)
		}
	}

	log.Printf("Started notifications for %d subscribers", len(subscribers))
}

func (app *App) sendTelegramNotifications(chatID int64, events []Event) {
	message := app.formatEventMessage(events)

	msg := tgbotapi.NewMessage(chatID, message)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = false
	if _, err := app.bot.Send(msg); err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
		if strings.Contains(err.Error(), "blocked") ||
			strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "kicked") {
			log.Printf("Removing blocked user %d", chatID)
			app.removeSubscriber(chatID)
			return
		}
	}
}

func (app *App) spamTelegramNotifications(chatID int64, events []Event, stopChan chan struct{}) {
	message := app.formatEventMessage(events)
	ticker := time.NewTicker(notifyInterval)
	defer ticker.Stop()

	timeout := time.After(spamDuration)

	for {
		select {
		case <-stopChan:
			log.Printf("Stopping notifications for chat %d", chatID)
			return
		case <-timeout:
			log.Printf("Notification timeout for chat %d", chatID)
			app.stopNotifyMu.Lock()
			delete(app.stopNotify, chatID)
			app.stopNotifyMu.Unlock()
			return
		case <-app.ctx.Done():
			return
		case <-ticker.C:
			msg := tgbotapi.NewMessage(chatID, message)
			msg.ParseMode = "HTML"
			msg.DisableWebPagePreview = false
			if _, err := app.bot.Send(msg); err != nil {
				log.Printf("Failed to send message to %d: %v", chatID, err)
				if strings.Contains(err.Error(), "blocked") ||
					strings.Contains(err.Error(), "not found") ||
					strings.Contains(err.Error(), "kicked") {
					log.Printf("Removing blocked user %d", chatID)
					app.removeSubscriber(chatID)
					return
				}
			}
		}
	}
}

func (app *App) formatEventMessage(events []Event) string {
	if len(events) == 1 {
		event := events[0]

		message := fmt.Sprintf("🎉 <b>New Sports Event!</b>\n\n<b>%s</b>\n", event.Title)

		if event.DateTime != "" {
			message += fmt.Sprintf("📅 %s\n", event.DateTime)
		}

		if event.Price != "" {
			message += fmt.Sprintf("💰 %s %s\n", event.Price, event.Currency)
		}

		message += fmt.Sprintf("\n🎫 <a href=\"%s\">Buy Tickets</a>", event.Href)
		return message
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🎉 <b>%d New Sports Events!</b>\n\n", len(events)))

	for i, event := range events {
		if i > 0 {
			sb.WriteString("\n➖➖➖➖➖➖➖➖\n")
		}
		sb.WriteString(fmt.Sprintf("<b>%s</b>\n", event.Title))

		if event.DateTime != "" {
			sb.WriteString(fmt.Sprintf("📅 %s\n", event.DateTime))
		}

		if event.Price != "" {
			sb.WriteString(fmt.Sprintf("💰 %s %s\n", event.Price, event.Currency))
		}

		sb.WriteString(fmt.Sprintf("🎫 <a href=\"%s\">Buy Tickets</a>\n", event.Href))
	}

	return sb.String()
}

func (app *App) listenForCommands() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := app.bot.GetUpdatesChan(u)

	log.Println("Started listening for Telegram commands")

	for {
		select {
		case <-app.ctx.Done():
			return
		case update := <-updates:
			if update.Message == nil {
				continue
			}

			chatID := update.Message.Chat.ID
			text := strings.ToLower(strings.TrimSpace(update.Message.Text))
			username := update.Message.From.UserName

			log.Printf("Received command from %s (ID: %d): %s", username, chatID, text)

			switch {
			case strings.HasPrefix(text, "/add ") && chatID == app.adminChatID:
				app.handleAddSubscriber(chatID, text)
			case strings.HasPrefix(text, "/remove ") && chatID == app.adminChatID:
				app.handleRemoveSubscriber(chatID, text)
			case text == "/list" && chatID == app.adminChatID:
				app.handleListSubscribers(chatID)
			case text == "/stop":
				app.handleStopNotifications(chatID)
			case text == "/status":
				app.handleStatus(chatID)
			case text == "/test":
				app.handleTest(chatID)
			case text == "/check":
				app.handleForceCheck(chatID)
			case text == "/help" || text == "/start":
				app.handleHelp(chatID)
			default:
				if chatID == app.adminChatID {
					app.sendMessage(chatID, "Unknown command. Use /help to see available commands.")
				}
			}
		}
	}
}

func (app *App) handleAddSubscriber(adminChatID int64, text string) {
	newIDStr := strings.TrimSpace(text[5:])
	newID, err := strconv.ParseInt(newIDStr, 10, 64)
	if err != nil {
		app.sendMessage(adminChatID, "❌ Invalid chat ID format")
		return
	}

	app.subscribersMu.Lock()
	// Check if already subscribed
	for _, id := range app.subscribers {
		if id == newID {
			app.subscribersMu.Unlock()
			app.sendMessage(adminChatID, fmt.Sprintf("ℹ️ Chat %d is already subscribed", newID))
			return
		}
	}

	app.subscribers = append(app.subscribers, newID)
	app.subscribersMu.Unlock()

	app.saveSubscriber(newID)
	app.sendMessage(adminChatID, fmt.Sprintf("✅ Added subscriber: %d", newID))
	app.sendMessage(newID, "🎉 You've been subscribed to Ticketon sports event notifications!\n\nUse /stop to unsubscribe anytime.")
	log.Printf("Added subscriber: %d", newID)
}

func (app *App) handleRemoveSubscriber(adminChatID int64, text string) {
	removeIDStr := strings.TrimSpace(text[8:])
	removeID, err := strconv.ParseInt(removeIDStr, 10, 64)
	if err != nil {
		app.sendMessage(adminChatID, "❌ Invalid chat ID format")
		return
	}

	if app.removeSubscriber(removeID) {
		app.sendMessage(adminChatID, fmt.Sprintf("✅ Removed subscriber: %d", removeID))
		app.sendMessage(removeID, "You've been unsubscribed from Ticketon notifications.")
	} else {
		app.sendMessage(adminChatID, fmt.Sprintf("❌ Subscriber %d not found", removeID))
	}
}

func (app *App) handleListSubscribers(adminChatID int64) {
	app.subscribersMu.RLock()
	count := len(app.subscribers)
	if count == 0 {
		app.subscribersMu.RUnlock()
		app.sendMessage(adminChatID, "📋 No subscribers")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 <b>Subscribers (%d):</b>\n\n", count))
	for i, id := range app.subscribers {
		sb.WriteString(fmt.Sprintf("%d. <code>%d</code>\n", i+1, id))
	}
	app.subscribersMu.RUnlock()

	msg := tgbotapi.NewMessage(adminChatID, sb.String())
	msg.ParseMode = "HTML"
	app.bot.Send(msg)
}

func (app *App) handleStopNotifications(chatID int64) {
	app.stopNotifyMu.Lock()
	if stopChan, exists := app.stopNotify[chatID]; exists {
		close(stopChan)
		delete(app.stopNotify, chatID)
		app.stopNotifyMu.Unlock()
		app.sendMessage(chatID, "🔕 Notifications stopped")
	} else {
		app.stopNotifyMu.Unlock()
		app.sendMessage(chatID, "ℹ️ No active notifications")
	}
}

func (app *App) handleStatus(adminChatID int64) {
	app.eventDataMu.RLock()
	eventCount := len(app.eventData.Events)
	lastCheck := app.eventData.LastCheck
	app.eventDataMu.RUnlock()

	app.subscribersMu.RLock()
	subCount := len(app.subscribers)
	app.subscribersMu.RUnlock()

	app.stopNotifyMu.RLock()
	activeNotifications := len(app.stopNotify)
	app.stopNotifyMu.RUnlock()

	status := fmt.Sprintf(`📊 <b>Bot Status:</b>

🎫 Events tracked: <code>%d</code>
👥 Subscribers: <code>%d</code>
🔔 Active notifications: <code>%d</code>
⏰ Last check: <code>%s</code>
🌐 Target URL: <code>%s</code>
⏱️ Check interval: <code>%v</code>`,
		eventCount, subCount, activeNotifications,
		lastCheck.Format("2006-01-02 15:04:05"),
		url, pollInterval)

	msg := tgbotapi.NewMessage(adminChatID, status)
	msg.ParseMode = "HTML"
	app.bot.Send(msg)
}

func (app *App) handleTest(adminChatID int64) {
	events := slices.Collect(maps.Values(app.eventData.Events))

	app.sendMessage(adminChatID, "🧪 Sending test notification...")

	testEvents := []Event{events[rand.Intn(len(events)-1)]}
	message := app.formatEventMessage(testEvents)

	msg := tgbotapi.NewMessage(adminChatID, message)
	msg.ParseMode = "HTML"
	app.bot.Send(msg)
}

func (app *App) handleForceCheck(adminChatID int64) {
	app.sendMessage(adminChatID, "🔄 Force checking for new events...")
	go func() {
		app.checkEvents()
		app.sendMessage(adminChatID, "✅ Force check completed")
	}()
}

func (app *App) handleHelp(chatID int64) {
	help := `🤖 <b>Ticketon Sports Event Checker</b>

Available commands:
🔕 /stop - Stop notifications for this chat
📊 /status - Show bot status
🧪 /test - Send test notification
🔄 /check - Force check for events
❓ /help - Show this help message`

	if chatID == app.adminChatID {
		help += `

<b>Admin commands:</b>
➕ /add &lt;chat_id&gt; - Add subscriber
➖ /remove &lt;chat_id&gt; - Remove subscriber
📋 /list - List all subscribers`
	}

	msg := tgbotapi.NewMessage(chatID, help)
	msg.ParseMode = "HTML"
	app.bot.Send(msg)
}

func (app *App) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := app.bot.Send(msg); err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
	}
}

func (app *App) removeSubscriber(chatID int64) bool {
	app.subscribersMu.Lock()
	defer app.subscribersMu.Unlock()

	for i, id := range app.subscribers {
		if id == chatID {
			app.subscribers = append(app.subscribers[:i], app.subscribers[i+1:]...)
			app.saveAllSubscribers()
			log.Printf("Removed subscriber: %d", chatID)
			return true
		}
	}
	return false
}

func (app *App) loadEventData() {
	app.eventData = &EventData{
		Events: make(map[string]Event),
	}

	file, err := os.Open(seenEventsFile)
	if err != nil {
		log.Println("No existing event data found, starting fresh")
		return
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(app.eventData); err != nil {
		log.Printf("Error loading event data: %v", err)
		app.eventData.Events = make(map[string]Event)
	}
}

func (app *App) saveEventData() {
	app.eventDataMu.RLock()
	defer app.eventDataMu.RUnlock()

	file, err := os.Create(seenEventsFile)
	if err != nil {
		log.Printf("Failed to save event data: %v", err)
		return
	}
	defer file.Close()

	if err := json.NewEncoder(file).Encode(app.eventData); err != nil {
		log.Printf("Failed to encode event data: %v", err)
	}
}

func (app *App) loadSubscribers() {
	file, err := os.Open(subscribersFile)
	if err != nil {
		log.Println("No existing subscribers found")
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		id, err := strconv.ParseInt(strings.TrimSpace(scanner.Text()), 10, 64)
		if err == nil {
			app.subscribers = append(app.subscribers, id)
		}
	}
}

func (app *App) saveSubscriber(chatID int64) {
	file, err := os.OpenFile(subscribersFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("Failed to save subscriber: %v", err)
		return
	}
	defer file.Close()
	fmt.Fprintln(file, chatID)
}

func (app *App) saveAllSubscribers() {
	file, err := os.Create(subscribersFile)
	if err != nil {
		log.Printf("Failed to save subscribers: %v", err)
		return
	}
	defer file.Close()

	for _, id := range app.subscribers {
		fmt.Fprintln(file, id)
	}
}
