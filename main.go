package main

import (
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
	"bytes"
)

//
// ------------------------------------------------------------
// CONFIG
// ------------------------------------------------------------
//

var (
	logFile = getEnv("LOG_FILE", "/data/logs/app.log")
	dataDir = getEnv("DATA_DIR", "/data/storage")

	// Mautic
	mauticURL  = os.Getenv("MAUTIC_URL")
	mauticUser = os.Getenv("MAUTIC_USER")
	mauticPass = os.Getenv("MAUTIC_PASS")

	// Fast2SMS WhatsApp
	fast2SMSURL   = getEnv("FAST2SMS_WHATSAPP_URL", "https://www.fast2sms.com/dev/whatsapp")
	fast2SMSKey   = os.Getenv("FAST2SMS_API_KEY")
	phoneNumberID = os.Getenv("PHONE_NUMBER_ID")

	msgOrderReceived = os.Getenv("MESSAGE_ID_ORDER_RECEIVED")
	msgOrderShipped  = os.Getenv("MESSAGE_ID_ORDER_SHIPPED")
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

func storeJSON(prefix, name string, data any) error {
	path := filepath.Join(dataDir, prefix)
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}

	file := filepath.Join(path, name+".json")
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(file, b, 0644)
}

// func storeJSON(folder, name string, payload any) {
// 	fileLock.Lock()
// 	defer fileLock.Unlock()

// 	dir := filepath.Join(dataDir, folder)
// 	_ = os.MkdirAll(dir, 0755)

// 	b, _ := json.MarshalIndent(payload, "", "  ")
// 	_ = os.WriteFile(filepath.Join(dir, name+".json"), b, 0644)
// }

//
// ------------------------------------------------------------
// IDEMPOTENCY
// ------------------------------------------------------------
//

func flagPath(orderID, state string) string {
	return filepath.Join(dataDir, "flags", orderID+"_"+state)
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

//
// ------------------------------------------------------------
// HANDLERS
// ------------------------------------------------------------
//

// GoKwik ABC handler
func rootHandler(w http.ResponseWriter, r *http.Request) {

	// Accept JSON only
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		logger.Printf("INFO | root | non-json request ignored")
		http.Error(w, "invalid content type", http.StatusBadRequest)
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		logger.Printf("ERROR | root | invalid json | err=%v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// ---- customer validation ----
	customerRaw, ok := payload["customer"]
	if !ok || customerRaw == nil {
		logger.Printf("ERROR | root | missing customer object")
		http.Error(w, "missing customer", http.StatusBadRequest)
		return
	}

	customer, ok := customerRaw.(map[string]any)
	if !ok {
		logger.Printf("ERROR | root | invalid customer object")
		http.Error(w, "invalid customer", http.StatusBadRequest)
		return
	}

	email, _ := customer["email"].(string)
	if email == "" {
		logger.Printf("ERROR | root | missing email")
		http.Error(w, "missing email", http.StatusBadRequest)
		return
	}

	firstName, _ := customer["firstname"].(string)
	lastName, _ := customer["lastname"].(string)
	phone, _ := customer["phone"].(string)

	// ---- cart validation ----
	cartRaw, ok := payload["cart"]
	if !ok || cartRaw == nil {
		logger.Printf("ERROR | root | missing cart object | email=%s", email)
		http.Error(w, "missing cart", http.StatusBadRequest)
		return
	}

	cart, ok := cartRaw.(map[string]any)
	if !ok {
		logger.Printf("ERROR | root | invalid cart object | email=%s", email)
		http.Error(w, "invalid cart", http.StatusBadRequest)
		return
	}

	cartURL, _ := cart["abc_url"].(string)
	cartValue, _ := cart["total_price"]
	dropStage, _ := cart["drop_stage"].(string)

	logger.Printf("INFO | gokwik payload received | email=%s", email)

	// ---- mautic payload ----
	mauticPayload := map[string]any{
		"email":                      email,
		"firstname":                  firstName,
		"lastname":                   lastName,
		"mobile":                     phone,
		"phone":                      phone,
		"lead_source":                "gokwik",
		"cart_url":                   cartURL,
		"cart_value":                 cartValue,
		"drop_stage":                 dropStage,
		"last_abandoned_cart_date":   nowISO(),
		"tags":                       "source:gokwik,intent:abandoned-cart",
		"abc_cupon5_sent":            false,
		"abc1":                       false,
		"abc2":                       false,
		"abc3":                       false,
	}

	if err := mauticUpsert(mauticPayload); err != nil {
		logger.Printf("ERROR | mautic upsert failed | email=%s | err=%v", email, err)
	} else {
		logger.Printf("INFO | mautic upsert success | email=%s", email)
	}

	if err := storeJSON("gokwik", email+"_"+time.Now().Format("150405"), payload); err != nil {
		logger.Printf("ERROR | failed to store gokwik payload | email=%s | err=%v", email, err)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// Website order data handler
func woocommerceHandler(w http.ResponseWriter, r *http.Request) {
	// var order map[string]any
	// _ = json.NewDecoder(r.Body).Decode(&order)

	// logger.Printf("DEBUG | Order Received - Raw Data - %s", order)

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

	logger.Printf(
		"INFO | woocommerce payload received | order_id=%s | status=%s",
		orderID,
		order["status"],
	)

	mauticPayload := map[string]any{
		"firstname": firstName,
		"lastname": lastName,
		"email": email,
		"mobile": phone,
		"phone": phone,
		"address1": truncate(addressLine1, 64),
		"address2": truncate(addressLine2, 64),
		"city": billing["city"],
		"zipcode": billing["postcode"],
		"last_order_id": orderID,
		"last_order_date": todayDDMMYYYY(),
		// "first_order_date": todayDDMMYYYY(),
		"last_order_value": order["total"],
		"has_purchased": true,
		"last_product_names": strings.Join(extractProducts(order), ", "),
		"lead_source": "woocommerce",
		"tags": []string{"source:website", "type:website-customer"},
		"abc_cupon5_sent": true,
		"abc1": true,
		"abc2": true,
		"abc3": true,
	}

	if err := mauticUpsert(mauticPayload); err != nil {
		logger.Printf("ERROR | mautic upsert failed | order_id=%s | err=%v", orderID, err)
	} else {
		logger.Printf("INFO | mautic upsert success | order_id=%s", orderID)
	}

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
			vars := fmt.Sprintf(
				"%s|%s|%s",
				billing["first_name"],
				orderID,
				todayDDMMYYYY(),
			)

			if err := sendWhatsApp(orderID, billing["phone"].(string), msgOrderShipped, vars, "fulfilled"); err != nil {
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
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		logger.Fatalf("fatal: failed to create data directory: %v", err)
	}

	// =========================================================
	// 3. Configure HTTP routes
	// ---------------------------------------------------------
	// Use net/http directly for clarity and predictability.
	// No middleware magic, no hidden behavior.
	// =========================================================
	mux := http.NewServeMux()

	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/woocommerce", woocommerceHandler)

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Explicit status is important for load balancers
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
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
		Addr:              ":8080",
		Handler:           mux,

		// Max time allowed to read request headers/body
		ReadTimeout:       10 * time.Second,

		// Max time allowed to read headers only
		ReadHeaderTimeout: 5 * time.Second,

		// Max time allowed to write response
		WriteTimeout:      15 * time.Second,

		// Max time to keep idle connections open
		IdleTimeout:       60 * time.Second,
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
