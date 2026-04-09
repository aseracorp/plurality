package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/product"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// PaymentInfo stores the extracted payment information
type PaymentInfo struct {
	CustomerEmail string `json:"customer_email"`
	ProductName   string `json:"product_name"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	PaymentStatus string `json:"payment_status"`
	PeriodEnd     int64  `json:"period_end"`
	UserId        string `json:"user_id"`
}

// HandleStripeWebhook processes Stripe webhook events
func HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if stripe.Key == "" {
		stripe.Key = os.Getenv("STRIPE_SECRET_KEY")
		if stripe.Key == "" {
			log.Println("Error: Stripe secret key not set")
			http.Error(w, "Unexpected Error", http.StatusInternalServerError)
		}
	}

	// Get webhook secret from environment
	endpointSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if endpointSecret == "" {
		log.Println("Warning: Stripe webhook secret not set")
	}

	// Read the request body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}

	// Get the Stripe-Signature header
	stripeSignature := r.Header.Get("Stripe-Signature")
	if stripeSignature == "" {
		log.Println("Missing Stripe-Signature header")
		http.Error(w, "Missing Stripe-Signature header", http.StatusBadRequest)
		return
	}

	// Verify the webhook signature
	var event stripe.Event
	if endpointSecret != "" {
		event, err = webhook.ConstructEvent(body, stripeSignature, endpointSecret)
		if err != nil {
			log.Printf("Webhook signature verification failed: %v", err)
			http.Error(w, "Webhook signature verification failed", http.StatusBadRequest)
			return
		}
	} else {
		// If webhook secret is not set (e.g., in development), parse the event without verification
		if err := json.Unmarshal(body, &event); err != nil {
			log.Printf("Error parsing webhook JSON: %v", err)
			http.Error(w, "Error parsing webhook JSON", http.StatusBadRequest)
			return
		}
	}

	// Process the event based on its type
	if event.Type == "invoice.paid" {
		handleInvoiceSucceeded(w, event)
	} else {
		// For other event types, just acknowledge receipt
		respondWithJSON(w, http.StatusOK, map[string]interface{}{
			"status": "received",
			"type":   event.Type,
		})
	}
}

func handleInvoiceSucceeded(w http.ResponseWriter, event stripe.Event) {
	// Parse the payment intent from the event data
	var paymentIntent stripe.Invoice
	err := json.Unmarshal(event.Data.Raw, &paymentIntent)
	if err != nil {
		log.Printf("Error parsing payment intent: %v", err)
		http.Error(w, "Error parsing payment intent", http.StatusBadRequest)
		return
	}

	// Extract product name and customer email
	productName := extractProductName(&paymentIntent)
	customerEmail := paymentIntent.CustomerEmail
	periodEnd := extractPeriodEnd(&paymentIntent)

	if customerEmail == "" {
		log.Println("Customer email not found")
		http.Error(w, "Customer email not found", http.StatusBadRequest)
		return
	}

	if productName == "" {
		log.Println("Product name not found")
		http.Error(w, "Product name not found", http.StatusBadRequest)
		return
	}

	customerId := utils.GetIdFromEmail(customerEmail)

	if customerId == "" {
		log.Println("Customer ID not found")
		http.Error(w, "Customer ID not found", http.StatusBadRequest)
		return
	}

	// Create payment info
	paymentInfo := PaymentInfo{
		CustomerEmail: customerEmail,
		ProductName:   productName,
		Amount:        paymentIntent.AmountPaid,
		Currency:      string(paymentIntent.Currency),
		PaymentStatus: string(paymentIntent.Status),
		PeriodEnd:     periodEnd,
		UserId:        customerId,
	}

	// Log the payment info
	log.Printf("Payment succeeded: %+v", paymentInfo)

	planAllowance := 0.0
	if strings.Contains(productName, "Plurality Basic") {
		planAllowance = 5000000
	} else if strings.Contains(productName, "Plurality Advanced") {
		planAllowance = 10000000
	} else if strings.Contains(productName, "Plurality Expert") {
		planAllowance = 22000000
	}

	// Update user plan
	err = db.UpdateUserPlan(customerId, productName, planAllowance, periodEnd)
	if err != nil {
		utils.Error("Error updating user plan", err)
		http.Error(w, "Error updating user plan", http.StatusInternalServerError)
		return
	}

	// Return a success response
	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "success",
		"payment_info": paymentInfo,
	})
}

// extractProductName attempts to extract the product name from the payment intent
func extractProductName(invoice *stripe.Invoice) string {
	// get first line item
	if len(invoice.Lines.Data) > 0 {
		lineItem := invoice.Lines.Data[0]
		if lineItem.Pricing != nil && lineItem.Pricing.PriceDetails != nil {
			productID := lineItem.Pricing.PriceDetails.Product
			productParams := &stripe.ProductParams{}
			productBought, err := product.Get(productID, productParams)
			if err != nil {
				log.Printf("Error fetching product details: %v", err)
			} else if productBought.Name != "" {
				return productBought.Name
			}
		}
	}

	return ""
}

func extractPeriodEnd(invoice *stripe.Invoice) int64 {
	// get first line item
	if len(invoice.Lines.Data) > 0 {
		lineItem := invoice.Lines.Data[0]
		if lineItem.Period != nil {
			return lineItem.Period.End
		}
	}
	return 0
}

// extractCustomerEmail attempts to extract the customer email from the payment intent
func extractCustomerEmail(paymentIntent *stripe.Invoice) string {

	// If we have a customer ID, fetch the customer details from Stripe
	if paymentIntent.Customer != nil && paymentIntent.Customer.ID != "" {
		// If the email is already available in the customer object
		if paymentIntent.Customer.Email != "" {
			return paymentIntent.Customer.Email
		}

		// Otherwise fetch the customer details from Stripe API
		customerID := paymentIntent.Customer.ID
		customerParams := &stripe.CustomerParams{}
		customer, err := customer.Get(customerID, customerParams)

		if err != nil {
			log.Printf("Error fetching customer details: %v", err)
		} else if customer.Email != "" {
			return customer.Email
		}

		// If we couldn't get the email, at least return the customer ID
		return ""
	}

	return ""
}

// respondWithJSON sends a JSON response
func respondWithJSON(w http.ResponseWriter, status int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling JSON: %v", err)
		http.Error(w, "Error creating JSON response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(response)
}
