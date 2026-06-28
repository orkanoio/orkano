package auth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestGenerateTOTP(t *testing.T) {
	secret, url, err := GenerateTOTP("Orkano", "admin")
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}
	if secret == "" {
		t.Fatal("GenerateTOTP returned an empty secret")
	}
	if url == "" {
		t.Fatal("GenerateTOTP returned an empty otpauth URL")
	}
}

func TestValidateTOTP(t *testing.T) {
	secret, _, err := GenerateTOTP("Orkano", "admin")
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}

	if !ValidateTOTP(secret, code) {
		t.Fatal("ValidateTOTP should accept a freshly generated code")
	}

	// Pick a wrong code that differs from the valid one regardless of which
	// value pquerna produced this run.
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if ValidateTOTP(secret, wrong) {
		t.Fatalf("ValidateTOTP should reject a wrong code %q", wrong)
	}

	if ValidateTOTP(secret, "not-a-code") {
		t.Fatal("ValidateTOTP should reject a malformed code")
	}
}
