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

	client := simpleforce.NewClient(
		envOrDefault("SF_URL", "https://test.salesforce.com"),
		envOrDefault("SF_CLIENT_ID", simpleforce.DefaultClientID),
		envOrDefault("SF_API_VERSION", simpleforce.DefaultAPIVersion),
	)
	if err := client.LoginPassword(username, password, os.Getenv("SF_TOKEN")); err != nil {
		log.Fatalf("sandbox login failed: %v", err)
	}
	if err := run(client); err != nil {
		log.Fatal(err)
	}
}

func run(client *simpleforce.Client) error {
	timestamp := time.Now().Format(time.RFC3339Nano)
	value := envOrDefault("SF_VALUE", "Bulk update test "+time.Now().Format(time.RFC3339))
	first := client.SObject("Account").
		Set("Name", "simpleforce bulk update test 1 "+timestamp).
		Create()
	if first == nil {
		return fmt.Errorf("failed to create first test Account")
	}
	defer deleteTestRecord(first)

	second := client.SObject("Account").
		Set("Name", "simpleforce bulk update test 2 "+timestamp).
		Create()
	if second == nil {
		return fmt.Errorf("failed to create second test Account")
	}
	defer deleteTestRecord(second)

	fmt.Printf("created test Accounts %s and %s\n", first.ID(), second.ID())

	records := []*simpleforce.SObject{
		client.SObject("Account").
			Set("Id", first.ID()).
			Set("Description", value),
		client.SObject("Account").
			Set("Id", second.ID()).
			Set("Name", nil),
	}

	results, err := client.Update(records, false)
	if err != nil {
		return fmt.Errorf("bulk update request failed: %w", err)
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
		return fmt.Errorf("expected at least one success and one failure; got %d successes and %d failures",
			successes, failures)
	}
	return nil
}

func deleteTestRecord(record *simpleforce.SObject) {
	if err := record.Delete(); err != nil {
		log.Printf("failed to delete test Account %s: %v", record.ID(), err)
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
