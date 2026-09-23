package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/simpleforce/simpleforce"
)

func main() {
	username := requiredEnv("SF_USER")
	password := requiredEnv("SF_PASS")
	recordID := requiredEnv("SF_RECORD_ID")

	client := simpleforce.NewClient(
		envOrDefault("SF_URL", "https://test.salesforce.com"),
		envOrDefault("SF_CLIENT_ID", simpleforce.DefaultClientID),
		envOrDefault("SF_API_VERSION", simpleforce.DefaultAPIVersion),
	)
	if err := client.LoginPassword(username, password, os.Getenv("SF_TOKEN")); err != nil {
		log.Fatalf("sandbox login failed: %v", err)
	}

	objectType := envOrDefault("SF_OBJECT_TYPE", "Account")
	field := envOrDefault("SF_FIELD", "Description")
	value := envOrDefault("SF_VALUE", "Bulk update test "+time.Now().Format(time.RFC3339))

	records := []*simpleforce.SObject{
		client.SObject(objectType).
			Set("Id", recordID).
			Set(field, value),
		client.SObject(objectType).
			Set("Id", recordID).
			Set("DefinitelyNotARealField__c", "intentional failure"),
	}

	results, err := client.Update(records, false)
	if err != nil {
		log.Fatalf("bulk update request failed: %v", err)
	}

	successes := 0
	failures := 0
	for index, result := range results {
		fmt.Printf("record %d: id=%s success=%t\n", index, result.ID, result.Success)
		if result.Success {
			successes++
			continue
		}

		failures++
		for _, resultError := range result.Errors {
			fmt.Printf("  error: statusCode=%s message=%q fields=%v\n",
				resultError.StatusCode, resultError.Message, resultError.Fields)
		}
	}

	if successes == 0 || failures == 0 {
		log.Fatalf("expected at least one success and one failure; got %d successes and %d failures",
			successes, failures)
	}
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func envOrDefault(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}
