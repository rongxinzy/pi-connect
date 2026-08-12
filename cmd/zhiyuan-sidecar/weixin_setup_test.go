package main

import "testing"

func TestWeixinSetupResponseDoesNotExposeManagementFields(t *testing.T) {
	response := weixinSetupResponse{Status: "confirmed", BotToken: "token", AccountID: "account"}
	if response.Status != "confirmed" || response.BotToken == "" || response.AccountID == "" {
		t.Fatal("unexpected setup response contract")
	}
}
