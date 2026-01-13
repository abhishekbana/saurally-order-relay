package main

import (
	"context"
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

	"github.com/google/uuid"
)

//
// -------------------------------------------------------------------
// Configuration
// -------------------------------------------------------------------
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

	msgOrderReceived = os.Getenv("MESSAGE_ID_ORDER_RECEIVED") // 10360
	msgOrderShipped  = os.Getenv("MESSAGE_ID_ORDER_SHIPPED")  // 10363
)

//
// -------------------------------------------------------------------
// Globals
// -------------------------------------------------------------------
//

var (
	logger   *log.Logger
	fileLock sync.Mutex
)

//
// -------------------------------------------------------------------
// Utilities
// -------------------------------------------------------------------
//

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func todayDDMMYYYY() string {
	return time.Now().Format("02/01/2006")
}

//
// -------------------------------------------------------------------
// Logger
// -------------------------------------------------------------------
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
// -------------------------------------------------------------------
// Storage Helpers
// -------------------------------------------------------------------
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
// -------------------------------------------------------------------
// Idempotency Flags
// -------------------------------------------------------------------
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
// -------------------------------------------------------------------
// Status Normalization
// -------------------------------------------------------------------
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
// -------------------------------------------------------------------
// Mautic Upsert
// -------------------------------------------------------------------
//

func mauticUpsert(payload map[string]any) error {
	if mauticURL == "" || mauticUser == "" || mauticPass == "" {
		return errors.New("mautic credentials missing")
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", mauticURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	req.SetBasicAuth(mauticUser, mauticPass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return errors.New(string(b))
	}

	return nil
}

//
// -------------------------------------------------------------------
// Fast2SMS WhatsApp
// -------------------------------------------------------------------
//

func sendWhatsAppTemplate(
	orderID string,
	phone string,
	templateID string,
	variables string,
	state string,
) error {

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

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return errors.New(string(body))
	}

	storeJSON("whatsapp", orderID+"_"+state, string(body))
	return nil
}

//
// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------
//

func extractProducts(order map[string]any) []string {
	items, ok := order["line_items"].([]any)
	if !ok {
		return nil
	}

	var products []string
	for _, i := range items {
		item := i.(map[string]any)
		products = append(products, item["name"].(string))
	}
	return products
}

//
// -------------------------------------------------------------------
// HTTP Handlers
// -------------------------------------------------------------------
//

/*
ROOT HANDLER – GOKWIK ABANDONED CART
Matches Python payload EXACTLY
*/
func rootHandler(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	customer := payload["customer"].(map[string]any)
	cart := payload["cart"].(map[string]any)

	email := customer["email"].(string)

	mauticPayload := map[string]any{
		"email":      email,
		"firstname":  customer["firstname"],
		"lastname":   customer["lastname"],
		"mobile":     customer["phone"],
		"lead_source": "gokwik",

		"cart_url":   cart["abc_url"],
		"cart_value": cart["total_price"],
		"drop_stage": cart["drop_stage"],

		"last_abandoned_cart_date": nowISO(),

		"tags": []string{
			"source:gokwik",
			"intent:abandoned-cart",
		},

		"abc_cupon5_sent": false,
		"abc1": false,
		"abc2": false,
		"abc3": false,
	}

	storeJSON("gokwik", uuid.New().String(), payload)
	err = mauticUpsert(mauticPayload)
	
	if err == nil {
		w.Write([]byte(`{"status":"ok"}`))
		logger.Printf(
			"INFO | mautic upsert abc successful | email=%s",
			email,
		)
	} else {
		logger.Printf(
			"ERROR | mautic upsert abc failed | email=%s | error=%s",
			email,
			err.Error(),
		)
	}
	
	// logger.Printf(
    // "INFO | gokwik payload received | email=%s",
    // email,
  	// )

	// w.Write([]byte(`{"status":"ok"}`))
}

/*
WOOCOMMERCE HANDLER – CUSTOMER PURCHASE
Matches Python payload EXACTLY
*/
func woocommerceHandler(w http.ResponseWriter, r *http.Request) {
	var order map[string]any
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	billing := order["billing"].(map[string]any)

	email := billing["email"].(string)
	phone := billing["phone"].(string)
	orderID := fmt.Sprintf("%v", order["id"])
	orderDate := todayDDMMYYYY()

	mauticPayload := map[string]any{
		"email": email,
		"mobile": phone,

		"last_order_id": orderID,
		"last_order_date": orderDate,
		"first_order_date": orderDate,

		"has_purchased": true,
		"last_product_names": extractProducts(order),

		"city": billing["city"],
		"pincode": billing["postcode"],

		"lead_source": "woocommerce",

		"tags": []string{
			"source:website",
			"type:website-customer",
		},

		"abc_cupon5_sent": true,
		"abc1": true,
		"abc2": true,
		"abc3": true,
	}

	storeJSON("woocommerce", uuid.New().String(), order)
	err = mauticUpsert(mauticPayload)
	
	if err == nil {
		w.Write([]byte(`{"status":"ok"}`))
		logger.Printf(
			"INFO | mautic upsert order successful | email=%s",
			email,
		)
	} else {
		logger.Printf(
			"ERROR | mautic upsert order failed | email=%s | error=%s",
			email,
			err.Error(),
		)
	}

	status := normalizeStatus(fmt.Sprintf("%v", order["status"]))

	switch status {

	case "processing":
		flag := flagPath(orderID, "processing")
		if !flagExists(flag) {
			vars := fmt.Sprintf(
				"%s|%s|%s|Rs. %v/-|%s",
				billing["first_name"],
				orderID,
				orderDate,
				order["total"],
				strings.ToUpper(order["payment_method_title"].(string)),
			)

			if err := sendWhatsAppTemplate(orderID, phone, msgOrderReceived, vars, "processing"); err == nil {
				createFlag(flag)
			}
		}

	case "fulfilled":
		flag := flagPath(orderID, "fulfilled")
		if !flagExists(flag) {
			vars := fmt.Sprintf(
				"%s|%s|%s",
				billing["first_name"],
				orderID,
				todayDDMMYYYY(),
			)

			if err := sendWhatsAppTemplate(orderID, phone, msgOrderShipped, vars, "fulfilled"); err == nil {
				createFlag(flag)
			} // add else error logging
		}
	}

	// w.Write([]byte(`{"status":"ok"}`))
}

//
// -------------------------------------------------------------------
// MAIN
// -------------------------------------------------------------------
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
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		logger.Println("INFO | server started on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal(err)
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
