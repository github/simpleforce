# simpleforce

A simple Golang client for Salesforce

[![GoDoc](https://godoc.org/github.com/github/simpleforce?status.svg)](https://godoc.org/github.com/github/simpleforce)

## Features

`simpleforce` is a library written in Go (Golang) that connects to Salesforce via the REST and Tooling APIs.
Currently, the following functions are implemented and more features could be added based on need:

- Execute SOQL queries
- Get records via record (sobject) type and ID
- Create records
- Update records
- Update multiple records in one API call
- Upsert multiple records in one API call
- Delete records
- Upsert (create or update) records based on an external ID
- Download a file
- Execute anonymous apex
- Send request to a custom Apex Rest endpoint

Most of the implementation referenced Salesforce documentation here: https://developer.salesforce.com/docs/atlas.en-us.214.0.api_rest.meta/api_rest/intro_what_is_rest_api.htm

## Installation

`simpleforce` can be acquired as any other Go libraries via `go get`:

```
go get github.com/github/simpleforce
```

## Quick Start

### Setup the Client

A `client` instance is the main entrance to access Salesforce using simpleforce. Create a `client` instance with the
`NewClient` function, with the proper endpoint URL:

```go
package main

import "github.com/github/simpleforce"

var (
	sfURL      = "Custom or instance URL, for example, 'https://na01.salesforce.com/'"
	sfUser     = "Username of the Salesforce account."
	sfPassword = "Password of the Salesforce account."
	sfToken    = "Security token, could be omitted if Trusted IP is configured."
)

func createClient() *simpleforce.Client {
	client := simpleforce.NewClient(sfURL, simpleforce.DefaultClientID, simpleforce.DefaultAPIVersion)
	if client == nil {
		// handle the error

		return nil
	}

	err := client.LoginPassword(sfUser, sfPassword, sfToken)
	if err != nil {
		// handle the error

		return nil
	}

	// Do some other stuff with the client instance if needed.

	return client
}
```

### Execute a SOQL Query

The `client` provides an interface to run an SOQL Query. Refer to
https://developer.salesforce.com/docs/atlas.en-us.api_rest.meta/api_rest/dome_query.htm
for more details about SOQL.

```go
package main

import (
    "fmt"
    "github.com/github/simpleforce"
)

func Query() {
	client := simpleforce.NewClient(...)
	client.LoginPassword(...)

	q := "Some SOQL Query String"
	result, err := client.Query(q) // Note: for Tooling API, use client.Tooling().Query(q)
	if err != nil {
		// handle the error
		return
	}

	for _, record := range result.Records {
		// access the record as SObjects.
		fmt.Println(record)
	}
}

```

### Streaming queries and field metadata

The GitHub fork uses the module/import path `github.com/github/simpleforce`.
Existing client methods retain their signatures and behavior. `QueryClient` is
an opt-in helper that shares an authenticated `*Client` and its HTTP transport:

```go
queries, err := simpleforce.NewQueryClient(
    func(ctx context.Context, rejectedSID string) (*simpleforce.Client, error) {
        // Return your existing authenticated client. When rejectedSID is not
        // empty, force a refresh of that rejected session, bypassing any TTL,
        // and return a NEW Client. Never mutate a previously returned Client.
        return authenticatedClient(ctx, rejectedSID)
    },
    simpleforce.QueryOptions{},
)
if err != nil {
    return err
}

err = queries.Query(ctx, "SELECT Id,Owner.Name,Amount FROM Opportunity",
    func(record map[string]interface{}) error {
        // JSON numbers are json.Number, including nested relationship values.
        return writeRecord(record)
    })
fields, err := queries.Describe(ctx, "Product2")
// fields contain Name, Type, Precision and Scale.
err = queries.BulkQuery(ctx, "SELECT Id,Name FROM Account", []string{"Id", "Name"},
    func(record map[string]string) error {
        return writeCSVRecord(record)
    })
usage := queries.Usage()
```

`Query` emits each REST page before fetching the next and validates `totalSize`.
Count-only `SELECT COUNT() FROM ...` queries are rejected before any HTTP call;
use the existing `Client.Query` API and read `QueryResult.TotalSize` instead.
Aggregates that return records, such as `COUNT(Id)`, are not count-only queries.
`BulkQuery` creates and polls a Bulk API 2.0 query job, then streams CSV records
without buffering a result page or table. CSV headers match requested fields
case-insensitively; result keys preserve requested spelling. Missing or duplicate
headers, malformed CSV, repeated/missing pagination locators, failed/aborted
jobs, and result count mismatches return errors. Treat a failed export as
incomplete: callbacks can run before a later validation error.
As with Go's `encoding/csv`, embedded CRLF sequences in quoted CSV fields are
normalized to LF. Use REST `Query` instead when exact multiline text preservation
is required.

Defaults are three transient retries, five-second polling, and 50,000 records per
Bulk page. `QueryOptions` can override these and inject a context-aware `Sleep`;
use `Client.SetHttpClient` to inject an HTTP client. GET requests retry transport
failures, 429 and 5xx responses with bounded exponential backoff (up to 32s);
`Retry-After` is honored up to 60s. Each HTTP request can force one refresh on 401.
Bulk creation is **not retried** after a transport failure, 429 or 5xx, because the
job may already exist. Response-body decoding/stream errors are returned rather
than replaying already emitted records.

Use a context deadline to bound polling, HTTP requests and backoff. Cancellation
and callback errors stop local processing immediately; they do not submit a
separate server-side job abort. The provider must be concurrency-safe and return
clients that remain immutable while in use. A refresh must return a new `Client`;
do not call `SetSidLoc`, `SetHttpClient`, login methods, or other mutating methods
on a client already returned to a query. A lock inside the provider alone cannot
protect concurrent request reads.
`Usage()` reports attempted REST/describe/Bulk-create/poll/result calls, transient
retries (excluding auth replays), and the latest nonempty `Sforce-Limit-Info`.
`BulkJobsCreated` counts creation attempts, not confirmed server-side jobs.

### Work with Records

`SObject` instances are created by `client` instance, either through the return values of `client.Query()`
or through `client.SObject()` directly. Once an `SObject` instance is created, it can be used to Create, Delete, Update,
or Get records through the Salesforce REST API. Here are some examples of using `SObject` instances to work on the
records.

```go
package main

import (
    "fmt"
    "github.com/github/simpleforce"
)

func WorkWithRecords() {
	client := simpleforce.NewClient(...)
	client.LoginPassword(...)

	// Get an SObject with given type and external ID
	obj := client.SObject("Case").Get("__ID__")
	if obj == nil {
		// Object doesn't exist, handle the error
		return
	}

	// Attributes are associated with all Salesforce returned SObjects, and can be accessed with the
	// `AttributesField` method.
	attrs := obj.AttributesField()
	if attrs != nil {
	 	fmt.Println(attrs.Type)    // "Case"
		fmt.Println(attrs.URL)     // "/services/data/v43.0/case/__ID__"
	}

	// Linked objects can be accessed with the `SObjectField` method.
	userObj := obj.SObjectField("User", "CreatedById")
	if userObj == nil {
		// Object doesn't exist, or field "CreatedById" is invalid.
		return
	}

	// Linked objects returned normally contains the type and ID field only. A `Get` operation is needed to
	// retrieve all the information of linked objects.
	fmt.Println(userObj.StringField("Name"))    // FAIL: fields are not populated.

	// If an SObject instance already has an ID (e.g. linked object), `Get` can retrieve the object directly without
	// parameter.
	userObj.Get()
	fmt.Println(userObj.StringField("Name"))    // SUCCESS: returns the name of the user.

	// For Update(), start with a blank SObject.
	// Set "Id" with an existing ID and any updated fields.
	//
	// Update() will return the updated object, or nil and print an error.
	updateObj := client.SObject("Contact").								// Create an empty object of type "Contact".
		Set("Id", "__ID__").											// Set the Id to an existing Contact ID.
		Set("FirstName", "New Name").									// Set any updated fields.
		Update()														// Update the record on Salesforce server.
	fmt.Println(updateObj)

	// Update up to 200 records in one API call. Results correspond to records by index.
	results, err := client.Update([]*simpleforce.SObject{
		client.SObject("Contact").
			Set("Id", "__CONTACT_ID__").
			Set("FirstName", "New Name"),
		client.SObject("Case").
			Set("Id", "__CASE_ID__").
			Set("Subject", "New Subject"),
	}, false)
	if err != nil {
		// The request failed before Salesforce could return per-record results.
		return
	}
	for _, result := range results {
		fmt.Println(result.ID, result.Success, result.Errors)
	}

	// For Upsert(), start with a blank SObject.
	// Upsert will create the object if it does not already exist and will update the object if it already exists.
	// Set "ExternalIDField" to the name of your external ID field
	// and populate that field with an your external ID and any updated fields.
	//
	// Upsert() will return the updated object, or nil and print an error.
	upsertObj := client.SObject("Contact").						// Create an empty object of type "Contact".
		Set("ExternalIDField", "customExtIdField__c").	// Set the ExternalIDField to the name of your external ID field.
		Set("customExtIdField__c", "__ExtID__").				// Set the specified ID field to your external ID.
		Set("FirstName", "New Name").										// Set any updated fields.
		Upsert()																				// Update the record on Salesforce server.
	fmt.Println(upsertObj)

	// Upsert up to 200 records of the same type in one API call. All records must use the same external ID field.
	upsertResults, err := client.Upsert([]*simpleforce.SObject{
		client.SObject("Contact").
			Set("ExternalIDField", "customExtIdField__c").
			Set("customExtIdField__c", "__EXT_ID_1__").
			Set("FirstName", "First Contact"),
		client.SObject("Contact").
			Set("ExternalIDField", "customExtIdField__c").
			Set("customExtIdField__c", 1002). // Numeric external IDs retain their JSON number type.
			Set("FirstName", "Second Contact"),
	}, false)
	if err != nil {
		// The request failed before Salesforce could return per-record results.
		return
	}
	for _, result := range upsertResults {
		fmt.Println(result.ID, result.Success, result.Created, result.Errors)
	}

	// Many SObject methods return the instance of the SObject, allowing chained access and operations to the
	// object. In the following example, all methods, except "Delete", returns *SObject so that the next method
	// can be invoked on the returned value directly.
	//
	// Delete() methods returns `error` instead, as Delete is supposed to delete the record from the server.
	err := client.SObject("Case").                               // Create an empty object of type "Case"
    		Set("Subject", "Case created by simpleforce").              // Set the "Subject" field.
	        Set("Comments", "Case commented by simpleforce").           // Set the "Comments" field.
    		Create().                                                   // Create the record on Salesforce server.
    		Get().                                                      // Refresh the fields from Salesforce server.
    		Delete()                                                    // Delete the record from Salesforce server.
	fmt.Println(err)
}
```

### Download a File

```go
// Setup client and login
// ...

// Get the content version ID
query := "SELECT Id FROM ContentVersion WHERE ContentDocumentId = '" + YOUR_CONTENTDOCUMENTID + "' AND IsLatest = true"
result, err = client.Query(query)
if err != nil {
    // handle the error
}
contentVersionID := ""
for _, record := range result.Records {
    contentVersionID = record.StringField("Id")
}

// Download file
downloadFilePath := "/absolute/path/to/yourfile.txt"

err = client.DownloadFile(contentVersionID, downloadFilePath)
if err != nil {
    // handle error
    return
}
```

### Execute Anonymous Apex

```go
// Setup client and login
// ...

result, err := client.ExecuteAnonymous("System.debug('test anonymous apex');")
if err != nil {
    // handle error
    return
}
```

## Development and Unit Test

A set of unit test cases are provided to validate the basic functions of simpleforce. Please do not run these
unit tests with a production instance of Salesforce as it would create, modify and delete data from the provided
Salesforce account.

The unit test requires a custom field `customExtIdField__c` to be present on the Type `Case` in your Salesforce setup.

### Sandbox Bulk Update Example

`examples/bulk-update` creates two temporary Accounts, then sends one valid update and one intentionally invalid update
in a single request. It prints the per-record results and deletes both test Accounts before exiting.

```sh
export SF_URL=https://test.salesforce.com
export SF_USER='sandbox-user@example.com.sandbox'
export SF_PASS='password'
export SF_TOKEN='security-token'

go run ./examples/bulk-update
```

`SF_VALUE` optionally sets the successful Account `Description` update. `SF_CLIENT_ID` and `SF_API_VERSION` are also
optional.

## License and Acknowledgement

This package is released under BSD license. Part of the code referenced the simple-salesforce
(https://github.com/simple-salesforce/simple-salesforce) project and the credit goes to Nick Catalano and the community
maintaining simple-salesforce.

Contributions are welcome, cheers :-)
