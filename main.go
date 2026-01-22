package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//
// ------------------------------------------------------------
// CONFIG
// ------------------------------------------------------------
//

var (
	logFile = getEnv("LOG_FILE", "/data/logs/app.log")
	// dataDir = getEnv("DATA_DIR", "/data/storage")

	dataDir    = "/data"
	storageDir = filepath.Join(dataDir, "storage")

	gokwikDir      = filepath.Join(storageDir, "gokwik")
	woocommerceDir = filepath.Join(storageDir, "woocommerce")
	whatsappDir    = filepath.Join(storageDir, "whatsapp")
	eventsDir      = filepath.Join(storageDir, "events")
	errorsDir      = filepath.Join(storageDir, "errors")

	// Mautic
	mauticURL  = os.Getenv("MAUTIC_URL")
	mauticUser = os.Getenv("MAUTIC_USER")
	mauticPass = os.Getenv("MAUTIC_PASS")

	// Fast2SMS WhatsApp
	fast2SMSURL   = getEnv("FAST2SMS_WHATSAPP_URL", "https://www.fast2sms.com/dev/whatsapp")
	fast2SMSKey   = os.Getenv("FAST2SMS_API_KEY")
	phoneNumberID = os.Getenv("PHONE_NUMBER_ID")

	msgOrderReceived            = os.Getenv("MESSAGE_ID_ORDER_RECEIVED")
	msgOrderShipped             = os.Getenv("MESSAGE_ID_ORDER_SHIPPED")
	msgOrderShippedWithTracking = os.Getenv("MESSAGE_ID_ORDER_SHIPPED_WITH_TRACKING")

	telegramToken        = os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatIDABC    = os.Getenv("TELEGRAM_CHAT_ID_ABC")
	telegramChatIDOrders = os.Getenv("TELEGRAM_CHAT_ID_ORDERS")
)

//
// ------------------------------------------------------------
// GLOBALS
// ------------------------------------------------------------
//

var (
	logger   *log.Logger
	fileLock sync.Mutex
)

//
// ------------------------------------------------------------
// UTILITIES
// ------------------------------------------------------------
//

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func todayDDMMYYYY() string {
	return time.Now().Format("02/01/2006")
}

//
// ------------------------------------------------------------
// LOGGER
// ------------------------------------------------------------
//

func initLogger() error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	mw := io.MultiWriter(os.Stdout, f)
	logger = log.New(mw, "", log.LstdFlags)
	logger.Println("INFO | logger initialized")
	return nil
}

//
// ------------------------------------------------------------
// STORAGE
// ------------------------------------------------------------
//

func storeJSON(category, name string, payload any) error {
	dir := filepath.Join(storageDir, category)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := filepath.Join(dir, name+".json")

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, b, 0644)
}

//
// ------------------------------------------------------------
// IDEMPOTENCY
// ------------------------------------------------------------
//

func flagPath(orderID, state string) string {
	return filepath.Join(storageDir, "flags", orderID+"_"+state)
}

func flagExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func createFlag(p string) {
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	_ = os.WriteFile(p, []byte(nowISO()), 0644)
}

//
// ------------------------------------------------------------
// STATUS
// ------------------------------------------------------------
//

func normalizeStatus(s string) string {
	switch strings.ToLower(s) {
	case "processing":
		return "processing"
	case "completed", "shipped":
		return "fulfilled"
	default:
		return ""
	}
}

//
// ------------------------------------------------------------
// MAUTIC
// ------------------------------------------------------------
//

func mauticUpsert(payload map[string]any) error {
	if mauticURL == "" {
		return errors.New("mautic url missing")
	}

	body, _ := json.Marshal(payload)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // for internal mautic
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	req, err := http.NewRequest("POST", mauticURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	req.SetBasicAuth(mauticUser, mauticPass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(b))
	}

	return nil
}

//
// ------------------------------------------------------------
// WHATSAPP
// ------------------------------------------------------------
//

func sendWhatsApp(orderID, phone, templateID, variables, state string) error {
	form := url.Values{}
	form.Set("message_id", templateID)
	form.Set("phone_number_id", phoneNumberID)
	form.Set("numbers", phone)
	form.Set("variables_values", variables)

	req, err := http.NewRequest(
		http.MethodPost,
		fast2SMSURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return err
	}

	req.Header.Set("authorization", fast2SMSKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return errors.New(string(b))
	}

	storeJSON("whatsapp", orderID+"_"+state, string(b))
	return nil
}

//
// ------------------------------------------------------------
// TELEGRAM
// ------------------------------------------------------------
//

func sendTelegram(message string, chatID string) {
	if os.Getenv("TELEGRAM_ENABLED") != "true" {
		return
	}

	// token := os.Getenv("TELEGRAM_BOT_TOKEN")
	// chatID := os.Getenv("TELEGRAM_CHAT_ID")

	if telegramToken == "" || chatID == "" {
		logger.Println("WARN | telegram | missing config")
		return
	}

	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/sendMessage",
		telegramToken,
	)

	payload := map[string]string{
		"chat_id":                  chatID,
		"text":                     message,
		"parse_mode":               "HTML",
		"disable_web_page_preview": "true",
	}

	body, _ := json.Marshal(payload)

	go func() {
		resp, err := http.Post(
			url,
			"application/json",
			bytes.NewBuffer(body),
		)
		if err != nil {
			logger.Printf("ERROR | telegram | send failed | err=%v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			logger.Printf(
				"ERROR | telegram | api error | status=%d | body=%s",
				resp.StatusCode,
				string(respBody),
			)
		}
	}()
}

//
// ------------------------------------------------------------
// HELPERS
// ------------------------------------------------------------
//

func extractProducts(order map[string]any) []string {
	items, ok := order["line_items"].([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, i := range items {
		m := i.(map[string]any)
		names = append(names, m["name"].(string))
	}
	return names
}

func getMap(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false
	}
	mv, ok := v.(map[string]any)
	return mv, ok
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// Extraxt shipment tracking number from order meta_data
func extractTrackingNumber(order map[string]any) string {
	metaRaw, ok := order["meta_data"].([]any)
	if !ok {
		return ""
	}

	for _, m := range metaRaw {
		meta, ok := m.(map[string]any)
		if !ok {
			continue
		}

		key, _ := meta["key"].(string)
		if key != "_wc_shipment_tracking_items" {
			continue
		}

		items, ok := meta["value"].([]any)
		if !ok || len(items) == 0 {
			continue
		}

		item, ok := items[0].(map[string]any)
		if !ok {
			continue
		}

		tracking, _ := item["tracking_number"].(string)
		return tracking
	}

	return ""
}

// isDuplicateEvent ensures each order+status is processed once
// Example key: order_51281_processing
func isDuplicateEvent(key string) bool {
	eventPath := filepath.Join(eventsDir, key)

	// Already processed
	if _, err := os.Stat(eventPath); err == nil {
		return true
	}

	// Ensure directory exists
	if err := os.MkdirAll(eventsDir, 0755); err != nil {
		logger.Printf("ERROR | events | mkdir failed | err=%v", err)
		return false // fail-open
	}

	// Persist marker
	_ = os.WriteFile(
		eventPath,
		[]byte(time.Now().Format(time.RFC3339)),
		0644,
	)

	return false
}

//
// ------------------------------------------------------------
// HANDLERS
// ------------------------------------------------------------
//

// GoKwik ABC handler (strict customer extraction + is_abandoned guard)
func abcHandler(w http.ResponseWriter, r *http.Request) {

	// Always respond OK to avoid GoKwik retries
	defer func() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}()

	// Accept JSON only
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		logger.Printf("INFO | abc | non-json request ignored")
		return
	}

	// ---- read raw body ----
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Printf("ERROR | abc | failed to read body | err=%v", err)
		return
	}

	logger.Printf("DEBUG | abc raw payload | %s", string(rawBody))

	// Restore body for decoding
	r.Body = io.NopCloser(bytes.NewBuffer(rawBody))

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		logger.Printf("ERROR | abc | invalid json | err=%v", err)
		return
	}

	// ---- carts extraction ----
	cartsRaw, ok := payload["carts"]
	if !ok {
		logger.Printf("ERROR | abc | carts key missing in payload")
		return
	}

	carts, ok := cartsRaw.([]any)
	if !ok || len(carts) == 0 {
		logger.Printf("ERROR | abc | carts empty or invalid")
		return
	}

	logger.Printf("INFO | abc | carts_received=%d", len(carts))

	// ---- process each cart ----
	for i, c := range carts {

		cart, ok := c.(map[string]any)
		if !ok {
			logger.Printf("ERROR | abc | cart[%d] invalid type", i)
			continue
		}

		// ---- is_abandoned flag ----
		isAbandoned, _ := cart["is_abandoned"].(bool)

		logger.Printf(
			"DEBUG | abc | is_abandoned raw=%v type=%T",
			cart["is_abandoned"],
			cart["is_abandoned"],
		)

		logger.Printf(
			"DEBUG | abc | cart flags | cart_id=%v is_abandoned=%v",
			cart["cart_id"],
			isAbandoned,
		)

		// ---- strict customer extraction ----
		customerRaw, ok := cart["customer"]
		if !ok {
			logger.Printf("ERROR | abc | cart[%d] missing customer object", i)
			continue
		}

		customer, ok := customerRaw.(map[string]any)
		if !ok {
			logger.Printf("ERROR | abc | cart[%d] customer invalid type", i)
			continue
		}

		email, _ := customer["email"].(string)
		phone, _ := customer["phone"].(string)
		firstName, _ := customer["firstname"].(string)
		lastName, _ := customer["lastname"].(string)

		if email == "" || phone == "" || firstName == "" {
			logger.Printf(
				"ERROR | abc | missing critical customer fields | email=%q phone=%q firstname=%q cart_id=%v",
				email,
				phone,
				firstName,
				cart["cart_id"],
			)
		}

		// ---- cart fields ----
		cartURL, _ := cart["abc_url"].(string)
		cartValue := cart["total_price"]
		dropStage, _ := cart["drop_stage"].(string)

		logger.Printf(
			"INFO | abc | cart processed | email=%s | drop_stage=%s | cart_id=%v",
			email,
			dropStage,
			cart["cart_id"],
		)

		// ---- mautic payload (always sent) ----
		mauticPayload := map[string]any{
			"email":                    email,
			"firstname":                firstName,
			"lastname":                 lastName,
			"mobile":                   phone,
			"phone":                    phone,
			"lead_source":              "gokwik",
			"cart_url":                 cartURL,
			"cart_value":               cartValue,
			"drop_stage":               dropStage,
			"last_abandoned_cart_date": nowISO(),
			"tags":                     "source:gokwik,intent:abandoned-cart",
			"abc_cupon5_sent":          false,
			"abc1":                     false,
			"abc2":                     false,
			"abc3":                     false,
		}

		if err := mauticUpsert(mauticPayload); err != nil {
			logger.Printf(
				"ERROR | abc | mautic upsert failed for ABC | email=%s | err=%v",
				email,
				err,
			)
		} else {
			logger.Printf("INFO | abc | mautic upsert success for ABC | email=%s", email)
		}

		// ---- extract cart items for Telegram ----
		itemsText := ""
		if itemsRaw, ok := cart["items"].([]any); ok && len(itemsRaw) > 0 {
			for _, it := range itemsRaw {
				item, ok := it.(map[string]any)
				if !ok {
					continue
				}

				title, _ := item["title"].(string)
				qtyFloat, _ := item["quantity"].(float64)

				if title != "" {
					itemsText += fmt.Sprintf("• %s × %d\n", title, int(qtyFloat))
				}
			}
		}

		if itemsText == "" {
			itemsText = "- (items unavailable)\n"
		}

		// ---- Telegram message (send ONLY if abandoned) ----
		if isAbandoned {

			telegramMessage := fmt.Sprintf(
				"🛒 <b>Abandoned Cart</b>\n\n"+
					"<b>Name:</b> %s %s\n"+
					"<b>Email:</b> %s\n"+
					"<b>Phone:</b> %s\n"+
					"<b>Cart Value:</b> ₹%v\n"+
					"<b>Stage:</b> %s\n\n"+
					"<b>Items:</b>\n%s\n"+
					"<a href=\"%s\">View Cart</a>",
				firstName,
				lastName,
				email,
				phone,
				cartValue,
				dropStage,
				itemsText,
				cartURL,
			)

			logger.Printf(
				"INFO | abc | sending telegram | email=%s | cart_id=%v",
				email,
				cart["cart_id"],
			)

			sendTelegram(telegramMessage, telegramChatIDABC)

		} else {
			logger.Printf(
				"INFO | abc | cart not abandoned | telegram skipped | cart_id=%v",
				cart["cart_id"],
			)
		}

		// ---- persist raw cart ----
		if err := storeJSON(
			"gokwik",
			fmt.Sprintf("%s_%s", email, time.Now().Format("150405")),
			cart,
		); err != nil {
			logger.Printf(
				"ERROR | abc | failed to store cart | email=%s | err=%v",
				email,
				err,
			)
		}
	}
}

// Website order data handler
func woocommerceHandler(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		logger.Printf(
			"INFO | woocommerce non-json webhook ignored | content-type=%s",
			ct,
		)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	// Read raw body
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Printf("ERROR | woocommerce | failed to read body | err=%v", err)
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Log raw JSON exactly as received
	logger.Printf("DEBUG | woocommerce raw payload | %s", string(rawBody))

	// Restore body for JSON decoding
	r.Body = io.NopCloser(bytes.NewBuffer(rawBody))

	var order map[string]any
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		logger.Printf("ERROR | woocommerce | json decode failed | err=%v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	billing, ok := getMap(order, "billing")
	if !ok {
		logger.Printf(
			"ERROR | woocommerce payload invalid | order_id=%v | reason=billing_missing",
			order["id"],
		)
		http.Error(w, "invalid payload: billing missing", http.StatusBadRequest)
		return
	}

	email, _ := billing["email"].(string)
	phone, _ := billing["phone"].(string)
	firstName, _ := billing["first_name"].(string)
	lastName, _ := billing["last_name"].(string)
	addressLine1, _ := billing["address_1"].(string)
	addressLine2, _ := billing["address_2"].(string)

	orderID := fmt.Sprintf("%v", order["id"])
	status := normalizeStatus(fmt.Sprintf("%v", order["status"]))

	eventKey := fmt.Sprintf("order_%s_%s", orderID, status)

	if isDuplicateEvent(eventKey) {
		logger.Printf(
			"INFO | woocommerce | duplicate event skipped | order_id=%s | status=%s",
			orderID,
			status,
		)
		return
	}

	logger.Printf(
		"INFO | woocommerce payload received | order_id=%s | status=%s",
		orderID,
		order["status"],
	)

	mauticPayload := map[string]any{
		"firstname":       firstName,
		"lastname":        lastName,
		"email":           email,
		"mobile":          phone,
		"phone":           phone,
		"address1":        truncate(addressLine1, 64),
		"address2":        truncate(addressLine2, 64),
		"city":            billing["city"],
		"zipcode":         billing["postcode"],
		"last_order_id":   orderID,
		"last_order_date": nowISO(),
		// "first_order_date": todayDDMMYYYY(),
		"last_order_value":   order["total"],
		"has_purchased":      true,
		"last_product_names": strings.Join(extractProducts(order), ", "),
		"lead_source":        "woocommerce",
		"tags":               []string{"source:website", "type:website-customer"},
		"abc_cupon5_sent":    true,
		"abc1":               true,
		"abc2":               true,
		"abc3":               true,
	}

	if err := mauticUpsert(mauticPayload); err != nil {
		logger.Printf("ERROR | mautic upsert failed for order | order_id=%s | err=%v", orderID, err)
	} else {
		logger.Printf("INFO | mautic upsert success for order | order_id=%s", orderID)
	}

	// construct telegram messgage and Send only if status is processing
	if status == "processing" {
		// ---- extract billing details ----
		billing, _ = order["billing"].(map[string]any)
		email, _ = billing["email"].(string)
		phone, _ = billing["phone"].(string)
		firstName, _ = billing["first_name"].(string)
		lastName, _ = billing["last_name"].(string)

		// ---- extract items ----
		itemsText := ""
		if itemsRaw, ok := order["line_items"].([]any); ok && len(itemsRaw) > 0 {
			for _, it := range itemsRaw {
				item, ok := it.(map[string]any)
				if !ok {
					continue
				}

				name, _ := item["name"].(string)
				qtyFloat, _ := item["quantity"].(float64)

				if name != "" {
					itemsText += fmt.Sprintf("- %s × %d\n", name, int(qtyFloat))
				}
			}
		}

		if itemsText == "" {
			itemsText = "- (items unavailable)\n"
		}

		// HTML formatting of telegram message
		telegramMessage := fmt.Sprintf(
			"📦 <b>New Order</b>\n\n"+
				"<b>Order ID:</b> %s\n"+
				"<b>Name:</b> %s %s\n"+
				"<b>Email:</b> %s\n"+
				"<b>Phone:</b> %s\n"+
				"<b>Amount:</b> ₹%s\n"+
				"<b>Payment:</b> %s\n\n"+
				"<b>Items:</b>\n%s",
			orderID,
			firstName,
			lastName,
			email,
			phone,
			order["total"],
			strings.ToUpper(order["payment_method_title"].(string)),
			itemsText,
		)
		sendTelegram(telegramMessage, telegramChatIDOrders)
	}

	// Send WhatsApp
	switch status {
	case "processing":
		flag := flagPath(orderID, "processing")
		if flagExists(flag) {
			logger.Printf("INFO | whatsapp skipped | order_id=%s | state=processing | reason=duplicate", orderID)
		} else {
			vars := fmt.Sprintf(
				"%s|%s|%s|Rs. %v/-|%s",
				billing["first_name"],
				orderID,
				todayDDMMYYYY(),
				order["total"],
				strings.ToUpper(order["payment_method_title"].(string)),
			)

			if err := sendWhatsApp(orderID, billing["phone"].(string), msgOrderReceived, vars, "processing"); err != nil {
				logger.Printf("ERROR | whatsapp failed | order_id=%s | state=processing | err=%v", orderID, err)
			} else {
				createFlag(flag)
				logger.Printf("INFO | whatsapp sent | order_id=%s | state=processing", orderID)
			}
		}

	case "fulfilled":
		flag := flagPath(orderID, "fulfilled")
		if flagExists(flag) {
			logger.Printf("INFO | whatsapp skipped | order_id=%s | state=fulfilled | reason=duplicate", orderID)
		} else {
			var messageID, vars string
			trackingNumber := extractTrackingNumber(order)
			// Prepare variables based on presence of tracking number
			// if tracking number is present, use template with tracking else template without tracking
			if trackingNumber == "" {
				vars = fmt.Sprintf(
					"%s|%s|%s",
					billing["first_name"],
					orderID,
					todayDDMMYYYY(),
				)
				messageID = msgOrderShipped
			} else {
				vars = fmt.Sprintf(
					"%s|%s|%s|%s",
					billing["first_name"],
					orderID,
					todayDDMMYYYY(),
					trackingNumber,
				)
				messageID = msgOrderShippedWithTracking
			}
			if err := sendWhatsApp(orderID, billing["phone"].(string), messageID, vars, "fulfilled"); err != nil {
				logger.Printf("ERROR | whatsapp failed | order_id=%s | state=fulfilled | err=%v", orderID, err)
			} else {
				createFlag(flag)
				logger.Printf("INFO | whatsapp sent | order_id=%s | state=fulfilled", orderID)
			}
		}
	}

	storeJSON("woocommerce", orderID, order)
	w.Write([]byte(`{"status":"ok"}`))
}

//
// ------------------------------------------------------------
// MAIN
// ------------------------------------------------------------
//

func main() {

	// =========================================================
	// 1. Initialize logger FIRST
	// ---------------------------------------------------------
	// Logging must be available before anything else,
	// otherwise startup failures are invisible.
	// =========================================================
	if err := initLogger(); err != nil {
		// Fallback to stderr because file logger failed
		log.Fatalf("fatal: failed to initialize logger: %v", err)
	}

	logger.Println("INFO | application starting")

	// =========================================================
	// 2. Prepare required filesystem structure
	// ---------------------------------------------------------
	// Ensure persistent directories exist BEFORE handling
	// any traffic. If this fails, the app must not start.
	// =========================================================
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		logger.Fatalf("fatal: failed to create data directory: %v", err)
	}

	// =========================================================
	// 3. Configure HTTP routes
	// ---------------------------------------------------------
	// Use net/http directly for clarity and predictability.
	// No middleware magic, no hidden behavior.
	// =========================================================
	mux := http.NewServeMux()

	mux.HandleFunc("/abc", abcHandler)
	mux.HandleFunc("/woocommerce", woocommerceHandler)

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// just in case if I wish to use a load balancer
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Blocks all requests at root or unknown paths
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("INFO | blocked root request | ip=%s | path=%s", r.RemoteAddr, r.URL.Path)
		http.NotFound(w, r)
	})

	// =========================================================
	// 4. Create hardened HTTP server
	// ---------------------------------------------------------
	// Timeouts are NOT optional for internet-facing services.
	// These protect against:
	// - slow clients
	// - hung connections
	// - resource exhaustion
	// =========================================================
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		// Max time allowed to read request headers/body
		ReadTimeout: 10 * time.Second,

		// Max time allowed to read headers only
		ReadHeaderTimeout: 5 * time.Second,

		// Max time allowed to write response
		WriteTimeout: 15 * time.Second,

		// Max time to keep idle connections open
		IdleTimeout: 60 * time.Second,
	}

	// Channel used to capture fatal server startup/runtime errors
	serverErr := make(chan error, 1)

	// =========================================================
	// 5. Start HTTP server asynchronously
	// ---------------------------------------------------------
	// The main goroutine remains free to listen for OS signals.
	// =========================================================
	go func() {
		logger.Println("INFO | server listening on :8080")

		// ListenAndServe only returns on error or shutdown
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// =========================================================
	// 6. Listen for termination signals
	// ---------------------------------------------------------
	// SIGTERM is what Docker/Kubernetes sends.
	// SIGINT is Ctrl+C (local/dev).
	// =========================================================
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// =========================================================
	// 7. Block until shutdown condition
	// ---------------------------------------------------------
	// We exit on:
	// - OS shutdown signal
	// - Fatal server error
	// =========================================================
	select {

	case sig := <-stop:
		logger.Printf("INFO | shutdown requested | signal=%s", sig)

	case err := <-serverErr:
		// This indicates a startup or runtime failure
		logger.Fatalf("FATAL | http server error | err=%v", err)
	}

	// =========================================================
	// 8. Graceful shutdown
	// ---------------------------------------------------------
	// Allow in-flight requests to finish.
	// New connections are rejected immediately.
	// =========================================================
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Printf("ERROR | graceful shutdown failed | err=%v", err)
	} else {
		logger.Println("INFO | server shutdown completed cleanly")
	}

	logger.Println("INFO | application stopped")
}
