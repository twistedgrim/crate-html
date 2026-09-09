package s3store

import (
	"strings"
	"testing"
)

func TestS3CredentialsPreferExplicitConfig(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-key")

	creds, err := s3Credentials(Config{
		AccessKey: "crate-access-key",
		SecretKey: "crate-secret-key",
	})
	if err != nil {
		t.Fatalf("s3Credentials: %v", err)
	}
	value, err := creds.Get()
	if err != nil {
		t.Fatalf("credentials.Get: %v", err)
	}
	if value.AccessKeyID != "crate-access-key" || value.SecretAccessKey != "crate-secret-key" {
		t.Errorf("credentials = %q/%q, want explicit CRATE_S3 credentials", value.AccessKeyID, value.SecretAccessKey)
	}
}

func TestS3CredentialsFailClearlyWhenChainIsEmpty(t *testing.T) {
	for _, key := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_ACCESS_KEY",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SECRET_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_ROLE_ARN",
		"AWS_ROLE_SESSION_NAME",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/missing")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://127.0.0.1:1/credentials")

	_, err := s3Credentials(Config{})
	if err == nil || !strings.Contains(err.Error(), "no AWS credentials found") {
		t.Fatalf("s3Credentials error = %v, want no AWS credentials found", err)
	}
}
