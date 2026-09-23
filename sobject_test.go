package simpleforce

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSObject_AttributesField(t *testing.T) {
	obj := &SObject{}
	if obj.AttributesField() != nil {
		t.Fail()
	}

	obj.setType("Case")
	if obj.AttributesField().Type != "Case" {
		t.Fail()
	}

	obj.setType("")
	if obj.AttributesField().Type != "" {
		t.Fail()
	}
}

func TestSObject_Type(t *testing.T) {
	obj := &SObject{
		sobjectAttributesKey: SObjectAttributes{Type: "Case"},
	}
	if obj.Type() != "Case" {
		t.Fail()
	}

	obj.setType("CaseComment")
	if obj.Type() != "CaseComment" {
		t.Fail()
	}
}

func TestSObject_InterfaceField(t *testing.T) {
	obj := &SObject{}
	if obj.InterfaceField("test_key") != nil {
		t.Fail()
	}

	(*obj)["test_key"] = "hello"
	if obj.InterfaceField("test_key") == nil {
		t.Fail()
	}
}

func TestSObject_SObjectField(t *testing.T) {
	obj := &SObject{
		sobjectAttributesKey: SObjectAttributes{Type: "CaseComment"},
		"ParentId":           "__PARENT_ID__",
	}

	// Positive checks
	caseObj := obj.SObjectField("Case", "ParentId")
	if caseObj.Type() != "Case" {
		log.Println("Type mismatch")
		t.Fail()
	}
	if caseObj.StringField("Id") != "__PARENT_ID__" {
		log.Println("ID mismatch")
		t.Fail()
	}

	// Negative checks
	userObj := obj.SObjectField("User", "OwnerId")
	if userObj != nil {
		log.Println("Nil mismatch")
		t.Fail()
	}
}

func TestSObject_Describe(t *testing.T) {
	client := requireClient(t, true)
	meta := client.SObject("Case").Describe()
	if meta == nil {
		t.FailNow()
	} else {
		if (*meta)["name"].(string) != "Case" {
			t.Fail()
		}
	}
}

func TestSObject_Get(t *testing.T) {
	client := requireClient(t, true)

	// Search for a valid Case ID first.
	queryResult, err := client.Query("SELECT Id,OwnerId,Subject FROM CASE")
	if err != nil || queryResult == nil {
		log.Println(logPrefix, "query failed,", err)
		t.FailNow()
	}
	if queryResult.TotalSize < 1 {
		t.FailNow()
	}
	oid := queryResult.Records[0].ID()
	ownerID := queryResult.Records[0].StringField("OwnerId")

	// Positive
	obj := client.SObject("Case").Get(oid)
	if obj.ID() != oid || obj.StringField("OwnerId") != ownerID {
		t.Fail()
	}

	// Positive 2
	obj = client.SObject("Case")
	if obj.StringField("OwnerId") != "" {
		t.Fail()
	}
	obj.setID(oid)
	obj.Get()
	if obj.ID() != oid || obj.StringField("OwnerId") != ownerID {
		t.Fail()
	}

	// Negative 1
	obj = client.SObject("Case").Get("non-exist-id")
	if obj != nil {
		t.Fail()
	}

	// Negative 2
	obj = &SObject{}
	if obj.Get() != nil {
		t.Fail()
	}
}

func TestSObject_Create(t *testing.T) {
	client := requireClient(t, true)

	// Positive
	case1 := client.SObject("Case")
	case1Result := case1.Set("Subject", "Case created by simpleforce on "+time.Now().Format("2006/01/02 03:04:05")).
		Set("Comments", "This case is created by simpleforce").
		Create()
	if case1Result == nil || case1Result.ID() == "" || case1Result.Type() != case1.Type() {
		t.Fail()
	} else {
		log.Println(logPrefix, "Case created,", case1Result.Get().StringField("CaseNumber"))
	}

	// Positive 2
	caseComment1 := client.SObject("CaseComment")
	caseComment1Result := caseComment1.Set("ParentId", case1Result.ID()).
		Set("CommentBody", "This comment is created by simpleforce & used for testing").
		Set("IsPublished", true).
		Create()
	if caseComment1Result.Get().SObjectField("Case", "ParentId").ID() != case1Result.ID() {
		t.Fail()
	} else {
		log.Println(logPrefix, "CaseComment created,", caseComment1Result.ID())
	}

	// Negative: object without type.
	obj := client.SObject()
	if obj.Create() != nil {
		t.Fail()
	}

	// Negative: object without client.
	obj = &SObject{}
	if obj.Create() != nil {
		t.Fail()
	}

	// Negative: Invalid type
	obj = client.SObject("__SOME_INVALID_TYPE__")
	if obj.Create() != nil {
		t.Fail()
	}

	// Negative: Invalid field
	obj = client.SObject("Case").Set("__SOME_INVALID_FIELD__", "")
	if obj.Create() != nil {
		t.Fail()
	}
}

func TestSObject_Update(t *testing.T) {
	client := requireClient(t, true)

	// Positive
	if client.SObject("Case").
		Set("Subject", "Case created by simpleforce on "+time.Now().Format("2006/01/02 03:04:05")).
		Create().
		Set("Subject", "Case subject updated by simpleforce").
		Update().
		Get().
		StringField("Subject") != "Case subject updated by simpleforce" {
		t.Fail()
	}
}

func TestClient_Update(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", request.Method)
		}
		if request.URL.Path != "/services/data/v54.0/composite/sobjects/" {
			t.Errorf("path = %s, want /services/data/v54.0/composite/sobjects/", request.URL.Path)
		}

		var body struct {
			AllOrNone bool                     `json:"allOrNone"`
			Records   []map[string]interface{} `json:"records"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body.AllOrNone {
			t.Error("allOrNone = false, want true")
		}
		if len(body.Records) != 2 {
			t.Fatalf("record count = %d, want 2", len(body.Records))
		}

		first := body.Records[0]
		attributes, ok := first["attributes"].(map[string]interface{})
		if !ok || attributes["type"] != "Contact" {
			t.Errorf("attributes = %#v, want Contact type", first["attributes"])
		}
		if len(attributes) != 1 {
			t.Errorf("attributes = %#v, want only type", attributes)
		}
		if first["id"] != "003000000000001" || first["FirstName"] != "Ada" {
			t.Errorf("first record = %#v", first)
		}
		if _, ok := first[sobjectClientKey]; ok {
			t.Error("request contains private client metadata")
		}
		if _, ok := first[sobjectIDKey]; ok {
			t.Error("request contains duplicate Id field")
		}
		if _, ok := first["LastModifiedDate"]; ok {
			t.Error("request contains read-only field")
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[
			{"id":"003000000000001","success":true,"errors":[]},
			{"id":"500000000000001","success":false,"errors":[
				{"statusCode":"INVALID_FIELD","message":"Invalid field","fields":["Subject"]}
			]}
		]`))
	}))
	defer server.Close()

	client := NewClient(server.URL, DefaultClientID, DefaultAPIVersion)
	client.instanceURL = server.URL
	client.sessionID = "test-session"
	objects := []*SObject{
		client.SObject("Contact").
			Set("Id", "003000000000001").
			Set("FirstName", "Ada").
			Set("LastModifiedDate", "ignored"),
		client.SObject("Case").
			Set("Id", "500000000000001").
			Set("Subject", "Updated"),
	}

	results, err := client.Update(objects, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	if !results[0].Success || results[0].ID != "003000000000001" {
		t.Errorf("first result = %#v", results[0])
	}
	if results[1].Success || len(results[1].Errors) != 1 {
		t.Fatalf("second result = %#v", results[1])
	}
	resultError := results[1].Errors[0]
	if resultError.StatusCode != "INVALID_FIELD" ||
		resultError.Message != "Invalid field" ||
		len(resultError.Fields) != 1 ||
		resultError.Fields[0] != "Subject" {
		t.Errorf("second result error = %#v", resultError)
	}
}

func TestClient_UpdateValidation(t *testing.T) {
	client := NewClient(DefaultURL, DefaultClientID, DefaultAPIVersion)
	client.sessionID = "test-session"

	tests := []struct {
		name    string
		objects []*SObject
	}{
		{name: "empty"},
		{name: "nil object", objects: []*SObject{nil}},
		{name: "missing type", objects: []*SObject{client.SObject().Set("Id", "001")}},
		{name: "missing ID", objects: []*SObject{client.SObject("Account")}},
		{name: "too many", objects: make([]*SObject, maxSObjectCollectionSize+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.Update(test.objects, false); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestClient_UpdateSObjects(t *testing.T) {
	client := NewClient(DefaultURL, DefaultClientID, DefaultAPIVersion)

	if _, err := client.UpdateSObjects(nil, false); err != ErrAuthentication {
		t.Fatalf("error = %v, want %v", err, ErrAuthentication)
	}
}

func TestSObject_Upsert(t *testing.T) {
	client := requireClient(t, true)

	// Positive create new object through upsert
	case1 := client.SObject("Case")
	case1Result := case1.Set("Subject", "Case created by simpleforce on "+time.Now().Format("2006/01/02 03:04:05")).
		Set("Comments", "This case is created by simpleforce").
		Set("customExtIdField__c", uuid.NewString()).
		Set("ExternalIDField", "customExtIdField__c").
		Upsert()
	if case1Result == nil || case1Result.ID() == "" || case1Result.Type() != case1.Type() {
		t.Fail()
	} else {
		log.Println(logPrefix, "Case created,", case1Result.Get().StringField("CaseNumber"))
	}

	// Positive update existing object through upsert
	case2 := client.SObject("Case").
		Set("Subject", "Case created by simpleforce on "+time.Now().Format("2006/01/02 03:04:05")).
		Set("customExtIdField__c", uuid.NewString())
	case2Result := case2.Create()
	case2.
		Set("Subject", "Case subject updated by simpleforce").
		Set("ExternalIDField", "customExtIdField__c").
		Upsert()
	if case2Result.Get().StringField("Subject") != "Case subject updated by simpleforce" {
		t.Fail()
	} else {
		log.Println(logPrefix, "Case updated,", case2Result.Get().StringField("CaseNumber"))
	}

	// Negative: object without type.
	obj := client.SObject()
	if obj.Upsert() != nil {
		t.Fail()
	}

	// Negative: object without client.
	obj = &SObject{}
	if obj.Upsert() != nil {
		t.Fail()
	}

	// Negative: Invalid type
	obj = client.SObject("__SOME_INVALID_TYPE__").
		Set("ExternalIDField", "customExtIdField__c").
		Set("customExtIdField__c", uuid.NewString())
	if obj.Upsert() != nil {
		t.Fail()
	}

	// Negative: Invalid field
	obj = client.SObject("Case").
		Set("ExternalIDField", "customExtIdField__c").
		Set("customExtIdField__c", uuid.NewString()).
		Set("__SOME_INVALID_FIELD__", "")
	if obj.Upsert() != nil {
		t.Fail()
	}

	// Negative: Missing ext ID
	obj = client.SObject("Case").
		Set("ExternalIDField", "customExtIdField__c")
	if obj.Upsert() != nil {
		t.Fail()
	}
}

func TestClient_Upsert(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", request.Method)
		}
		if request.URL.Path != "/services/data/v54.0/composite/sobjects/Account/External_Key__c" {
			t.Errorf(
				"path = %s, want /services/data/v54.0/composite/sobjects/Account/External_Key__c",
				request.URL.Path,
			)
		}

		var body struct {
			AllOrNone bool                     `json:"allOrNone"`
			Records   []map[string]interface{} `json:"records"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AllOrNone {
			t.Error("allOrNone = true, want false")
		}
		if len(body.Records) != 2 {
			t.Fatalf("record count = %d, want 2", len(body.Records))
		}

		first := body.Records[0]
		attributes, ok := first["attributes"].(map[string]interface{})
		if !ok || attributes["type"] != "Account" {
			t.Errorf("attributes = %#v, want Account type", first["attributes"])
		}
		if first["External_Key__c"] != "account-1" || first["Name"] != "First account" {
			t.Errorf("first record = %#v", first)
		}
		if _, ok := first[sobjectExternalIDFieldNameKey]; ok {
			t.Error("request contains private ExternalIDField metadata")
		}
		if _, ok := first[sobjectIDKey]; ok {
			t.Error("request contains Salesforce record ID")
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[
			{"id":"001000000000001","success":true,"created":true,"errors":[]},
			{"id":null,"success":false,"created":false,"errors":[
				{"statusCode":"INVALID_FIELD","message":"Invalid field","fields":["Invalid_Field__c"]}
			]}
		]`))
	}))
	defer server.Close()

	client := NewClient(server.URL, DefaultClientID, DefaultAPIVersion)
	client.instanceURL = server.URL
	client.sessionID = "test-session"
	objects := []*SObject{
		client.SObject("Account").
			Set("ExternalIDField", "External_Key__c").
			Set("External_Key__c", "account-1").
			Set("Name", "First account"),
		client.SObject("Account").
			Set("ExternalIDField", "External_Key__c").
			Set("External_Key__c", "account-2").
			Set("Invalid_Field__c", "intentional failure"),
	}

	results, err := client.Upsert(objects, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	if !results[0].Success || !results[0].Created || results[0].ID != "001000000000001" {
		t.Errorf("first result = %#v", results[0])
	}
	if results[1].Success || results[1].Created || len(results[1].Errors) != 1 {
		t.Fatalf("second result = %#v", results[1])
	}
	resultError := results[1].Errors[0]
	if resultError.StatusCode != "INVALID_FIELD" ||
		resultError.Message != "Invalid field" ||
		len(resultError.Fields) != 1 ||
		resultError.Fields[0] != "Invalid_Field__c" {
		t.Errorf("second result error = %#v", resultError)
	}
}

func TestClient_UpsertValidation(t *testing.T) {
	client := NewClient(DefaultURL, DefaultClientID, DefaultAPIVersion)
	client.sessionID = "test-session"

	valid := func() *SObject {
		return client.SObject("Account").
			Set("ExternalIDField", "External_Key__c").
			Set("External_Key__c", "account-1")
	}
	tests := []struct {
		name    string
		objects []*SObject
	}{
		{name: "empty"},
		{name: "nil object", objects: []*SObject{nil}},
		{name: "missing type", objects: []*SObject{
			client.SObject().
				Set("ExternalIDField", "External_Key__c").
				Set("External_Key__c", "account-1"),
		}},
		{name: "missing external ID field", objects: []*SObject{
			client.SObject("Account"),
		}},
		{name: "missing external ID value", objects: []*SObject{
			client.SObject("Account").Set("ExternalIDField", "External_Key__c"),
		}},
		{name: "mismatched type", objects: []*SObject{
			valid(),
			client.SObject("Contact").
				Set("ExternalIDField", "External_Key__c").
				Set("External_Key__c", "contact-1"),
		}},
		{name: "mismatched external ID field", objects: []*SObject{
			valid(),
			client.SObject("Account").
				Set("ExternalIDField", "Other_Key__c").
				Set("Other_Key__c", "account-2"),
		}},
		{name: "too many", objects: make([]*SObject, maxSObjectCollectionSize+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.Upsert(test.objects, false); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestClient_UpsertSObjects(t *testing.T) {
	client := NewClient(DefaultURL, DefaultClientID, DefaultAPIVersion)

	if _, err := client.UpsertSObjects(nil, false); err != ErrAuthentication {
		t.Fatalf("error = %v, want %v", err, ErrAuthentication)
	}
}

func TestSObject_Delete(t *testing.T) {
	client := requireClient(t, true)

	// Positive: create a case first then delete it and verify if it is gone.
	case1 := client.SObject("Case").
		Set("Subject", "Case created by simpleforce on "+time.Now().Format("2006/01/02 03:04:05")).
		Create().
		Get()
	if case1 == nil || case1.ID() == "" {
		t.Fatal()
	}
	caseID := case1.ID()
	if case1.Delete() != nil {
		t.Fail()
	}
	case1 = client.SObject("Case").Get(caseID)
	if case1 != nil {
		t.Fail()
	}
}

// TestSObject_GetUpdate validates updating of existing records.
func TestSObject_GetUpdate(t *testing.T) {
	client := requireClient(t, true)

	// Create a new case first.
	case1 := client.SObject("Case").
		Set("Subject", "Original").
		Create().
		Get()

	// Query the case by ID, then update the Subject.
	case2 := client.SObject("Case").
		Get(case1.ID()).
		Set("Subject", "Updated").
		Update().
		Get()

	// Query the case by ID again and check if the Subject has been updated.
	case3 := client.SObject("Case").
		Get(case2.ID())

	if case3.StringField("Subject") != "Updated" {
		t.Fail()
	}

	user1 := client.SObject("User").Create()
	log.Println(user1.ID())
}
