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

	logger = log.New(f, "", log.LstdFlags|log.LUTC)
	logger.Println("INFO | logger initialized")
	return nil
}

//
// ------------------------------------------------------------
// STORAGE
// ------------------------------------------------------------
//

func storeJSON(folder, name string, payload any) {
	fileLock.Lock()
	defer fileLock.Unlock()

	dir := filepath.Join(dataDir, folder)
	_ = os.MkdirAll(dir, 0755)

	b, _ := json.MarshalIndent(payload, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, name+".json"), b, 0644)
}

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

//
// ------------------------------------------------------------
// HANDLERS
// ------------------------------------------------------------
//

func rootHandler(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)

	customer := payload["customer"].(map[string]any)
	cart := payload["cart"].(map[string]any)
	email := customer["email"].(string)

	logger.Printf("INFO | gokwik payload received | email=%s", email)

	mauticPayload := map[string]any{
		"email": email,
		"firstname": customer["firstname"],
		"lastname": customer["lastname"],
		"mobile": customer["phone"],
		"lead_source": "gokwik",
		"cart_url": cart["abc_url"],
		"cart_value": cart["total_price"],
		"drop_stage": cart["drop_stage"],
		"last_abandoned_cart_date": nowISO(),
		"tags": []string{"source:gokwik", "intent:abandoned-cart"},
		"abc_cupon5_sent": false,
		"abc1": false,
		"abc2": false,
		"abc3": false,
	}

	if err := mauticUpsert(mauticPayload); err != nil {
		logger.Printf("ERROR | mautic upsert failed | email=%s | err=%v", email, err)
	} else {
		logger.Printf("INFO | mautic upsert success | email=%s", email)
	}

	storeJSON("gokwik", email+"_"+time.Now().Format("150405"), payload)
	w.Write([]byte(`{"status":"ok"}`))
}

func woocommerceHandler(w http.ResponseWriter, r *http.Request) {
	// var order map[string]any
	// _ = json.NewDecoder(r.Body).Decode(&order)

	// logger.Printf("DEBUG | Order Received - Raw Data - %s", order)

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
		"last_order_id": orderID,
		"last_order_date": todayDDMMYYYY(),
		// "first_order_date": todayDDMMYYYY(),
		"last_order_value": order["total"],
		"has_purchased": true,
		"last_product_names": strings.Join(extractProducts(order), ", "),
		"city": billing["city"],
		"pincode": billing["postcode"],
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
	if err := initLogger(); err != nil {
		log.Fatal(err)
	}

	_ = os.MkdirAll(dataDir, 0755)

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/woocommerce", woocommerceHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr: ":8080",
		Handler: mux,
	}

	go func() {
		logger.Println("INFO | server started on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	logger.Println("INFO | shutdown complete")
}
