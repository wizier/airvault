package service

import (
	"strings"
	"testing"
)

func TestClassifyActivationReply(t *testing.T) {
	record := []byte(`<?xml version="1.0"?><plist><dict/></plist>`)
	appleIDForm := []byte(`<xmlui><page><navigationBar title="Activate iPhone"/>` +
		`<tableView><section><editableTextRow id="login" label="Apple ID"/>` +
		`<editableTextRow id="password" label="Password" secure="true"/></section></tableView></page></xmlui>`)
	serverError := []byte(`<xmlui><navigationBar title="Activation Error"/></xmlui>`)
	alreadyActivated := []byte(`<xmlui><clientInfo ack-received="true"/></xmlui>`)
	cases := []struct {
		name        string
		contentType string
		body        []byte
		wantApply   bool
		wantCode    string
		wantErr     string
	}{
		{"session record", "text/xml", record, true, "", ""},
		{"record with charset", "application/xml; charset=UTF-8", record, true, "", ""},
		{"apple id form is activation lock", "application/x-buddyml", appleIDForm, false, errorCodeActivationLock, "Activation Lock"},
		{"error page keeps apple's title", "application/x-buddyml", serverError, false, errorCodeActivationFailed, "Activation Error"},
		{"ack means already activated", "application/x-buddyml", alreadyActivated, false, "", ""},
		{"unrecognized page is not lock", "application/x-buddyml", []byte("<xmlui/>"), false, errorCodeActivationFailed, "unrecognized"},
		{"empty reply", "text/xml", nil, false, errorCodeActivationFailed, "empty"},
		{"html error page", "text/html", []byte("<html/>"), false, errorCodeActivationFailed, "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyRecord, code, err := classifyActivationReply(tc.contentType, tc.body)
			if applyRecord != tc.wantApply || code != tc.wantCode || (err != nil) != (tc.wantErr != "") {
				t.Fatalf("classifyActivationReply(%q) = %v, %q, %v; want %v, %q, err=%v",
					tc.contentType, applyRecord, code, err, tc.wantApply, tc.wantCode, tc.wantErr != "")
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// A lockdown read failure must not erase the last known activation state.
func TestApplyActivationKeepsLastKnownOnFailedRead(t *testing.T) {
	store := newDeviceRuntimeStore()
	store.applyPresence(map[string]string{"udid-1": "wifi"})

	if !store.applyActivation("udid-1", "Unactivated") {
		t.Fatal("first real state should register as a change")
	}
	if store.applyActivation("udid-1", "") {
		t.Fatal("failed read must not count as a change")
	}
	if got := store.snapshot()["udid-1"].activation; got != "Unactivated" {
		t.Fatalf("activation = %q, want last known Unactivated", got)
	}
	if !store.applyActivation("udid-1", "Activated") {
		t.Fatal("real transition should register as a change")
	}
}
