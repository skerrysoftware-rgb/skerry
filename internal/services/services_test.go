package services

import (
	"strings"
	"testing"
)

func TestPasswordsDifferPerService(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	pg, vk := Password(secret, "postgres"), Password(secret, "valkey")
	if pg == vk || pg == secret || vk == secret {
		t.Error("services share a password, or use the workspace secret itself")
	}
	if Password(secret, "postgres") != pg || len(pg) != 64 {
		t.Errorf("password %q is not stable 64-hex", pg)
	}
	env := Env([]string{"postgres", "valkey"}, secret)
	if !strings.Contains(env["DATABASE_URL"], pg) || !strings.Contains(env["REDIS_URL"], vk) {
		t.Error("the environment doesn't use each service's own password")
	}
}
