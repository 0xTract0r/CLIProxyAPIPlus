package devin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestBuildGetUserStatusRequest(t *testing.T) {
	reqBytes := BuildGetUserStatusRequest("test-token-123")
	if len(reqBytes) == 0 {
		t.Fatal("expected non-empty request bytes")
	}

	num, wireType, n := protowire.ConsumeTag(reqBytes)
	if num != 1 || wireType != protowire.BytesType {
		t.Fatalf("expected Field 1 BytesType, got num=%d wireType=%d", num, wireType)
	}

	f1Bytes, _ := protowire.ConsumeBytes(reqBytes[n:])
	found := false
	for len(f1Bytes) != 0 {
		field, typ, consumed := protowire.ConsumeTag(f1Bytes)
		if consumed < 0 || typ != protowire.BytesType {
			t.Fatal("invalid selected grant metadata")
		}
		fieldValue, size := protowire.ConsumeBytes(f1Bytes[consumed:])
		if size < 0 {
			t.Fatal("invalid metadata field length")
		}
		switch field {
		case 3:
			found = string(fieldValue) == "test-token-123"
		case 4, 5:
			// Ordinary language and runtime OS.
		default:
			t.Fatalf("unexpected client or device identity field %d", field)
		}
		f1Bytes = f1Bytes[consumed+size:]
	}
	if !found {
		t.Fatal("selected grant missing")
	}
}

func buildMockUserStatusResponse() []byte {
	// PlanInfo
	var planInfo []byte
	planInfo = protowire.AppendTag(planInfo, 2, protowire.BytesType)
	planInfo = protowire.AppendString(planInfo, "Pro")

	var orgInfo []byte
	orgInfo = protowire.AppendTag(orgInfo, 4, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "org-test-123")
	orgInfo = protowire.AppendTag(orgInfo, 8, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "TestOrg")

	planInfo = protowire.AppendTag(planInfo, 33, protowire.BytesType)
	planInfo = protowire.AppendBytes(planInfo, orgInfo)

	// PlanStatus
	var planStatus []byte
	planStatus = protowire.AppendTag(planStatus, 1, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planInfo)

	// plan_start
	var planStart []byte
	planStart = protowire.AppendTag(planStart, 1, protowire.VarintType)
	planStart = protowire.AppendVarint(planStart, 1789087364)
	planStatus = protowire.AppendTag(planStatus, 2, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planStart)

	// plan_end
	var planEnd []byte
	planEnd = protowire.AppendTag(planEnd, 1, protowire.VarintType)
	planEnd = protowire.AppendVarint(planEnd, 1791679364)
	planStatus = protowire.AppendTag(planStatus, 3, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planEnd)

	// daily quota: 100%
	planStatus = protowire.AppendTag(planStatus, 14, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 100)

	// weekly quota: 50%
	planStatus = protowire.AppendTag(planStatus, 15, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 50)

	// daily reset: 1789200000
	planStatus = protowire.AppendTag(planStatus, 17, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789200000)

	// weekly reset: 1789286400
	planStatus = protowire.AppendTag(planStatus, 18, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789286400)

	// UserStatus
	var userStatus []byte
	userStatus = protowire.AppendTag(userStatus, 3, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "testuser")

	userStatus = protowire.AppendTag(userStatus, 5, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "team-123")

	userStatus = protowire.AppendTag(userStatus, 7, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "testuser@example.com")

	userStatus = protowire.AppendTag(userStatus, 13, protowire.BytesType)
	userStatus = protowire.AppendBytes(userStatus, planStatus)

	userStatus = protowire.AppendTag(userStatus, 36, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "user-id-456")

	// Top-level response: Field 1 is UserStatus
	var resp []byte
	resp = protowire.AppendTag(resp, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, userStatus)

	return resp
}

func TestParseGetUserStatusResponse(t *testing.T) {
	mockBytes := buildMockUserStatusResponse()

	status, err := ParseGetUserStatusResponse(mockBytes)
	if err != nil {
		t.Fatalf("ParseGetUserStatusResponse failed: %v", err)
	}

	if status.UserName != "testuser" {
		t.Errorf("expected UserName=testuser, got %q", status.UserName)
	}
	if status.Email != "testuser@example.com" {
		t.Errorf("expected Email=testuser@example.com, got %q", status.Email)
	}
	if status.UserID != "user-id-456" {
		t.Errorf("expected UserID=user-id-456, got %q", status.UserID)
	}
	if status.TeamID != "team-123" {
		t.Errorf("expected TeamID=team-123, got %q", status.TeamID)
	}
	if status.Plan != "Pro" {
		t.Errorf("expected Plan=Pro, got %q", status.Plan)
	}
	if status.OrgID != "org-test-123" {
		t.Errorf("expected OrgID=org-test-123, got %q", status.OrgID)
	}
	if status.OrgName != "TestOrg" {
		t.Errorf("expected OrgName=TestOrg, got %q", status.OrgName)
	}
	if status.DailyQuotaRemainingPercent != 100 {
		t.Errorf("expected DailyQuotaRemainingPercent=100, got %d", status.DailyQuotaRemainingPercent)
	}
	if status.WeeklyQuotaRemainingPercent != 50 {
		t.Errorf("expected WeeklyQuotaRemainingPercent=50, got %d", status.WeeklyQuotaRemainingPercent)
	}
	expectedDailyReset := time.Unix(1789200000, 0).UTC()
	if !status.DailyQuotaResetAt.Equal(expectedDailyReset) {
		t.Errorf("expected DailyQuotaResetAt=%v, got %v", expectedDailyReset, status.DailyQuotaResetAt)
	}
	expectedWeeklyReset := time.Unix(1789286400, 0).UTC()
	if !status.WeeklyQuotaResetAt.Equal(expectedWeeklyReset) {
		t.Errorf("expected WeeklyQuotaResetAt=%v, got %v", expectedWeeklyReset, status.WeeklyQuotaResetAt)
	}
}

func TestFetchUserStatusLiveMock(t *testing.T) {
	mockResp := buildMockUserStatusResponse()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DevinGetUserStatusPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Errorf("missing Connect-Protocol-Version header")
		}
		if r.Header.Get("Content-Type") != "application/proto" {
			t.Errorf("expected Content-Type application/proto, got %s", r.Header.Get("Content-Type"))
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Basic mytoken-mytoken" {
			t.Errorf("unexpected Authorization header: %s", authHeader)
		}

		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mockResp)
	}))
	defer server.Close()

	svc := NewDevinAuthService(server.Client())
	svc.SetServerBaseURL(server.URL)

	status, err := svc.FetchUserStatus(context.Background(), "mytoken")
	if err != nil {
		t.Fatalf("FetchUserStatus failed: %v", err)
	}

	if status.Plan != "Pro" {
		t.Errorf("expected plan Pro, got %s", status.Plan)
	}
	if status.DailyQuotaRemainingPercent != 100 {
		t.Errorf("expected daily quota 100, got %d", status.DailyQuotaRemainingPercent)
	}
}

type ordinaryDevinStatusTransport func(*http.Request) (*http.Response, error)

func (transport ordinaryDevinStatusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestOrdinaryDevinUserStatusRefusalPreservesSelectedGrant(t *testing.T) {
	const grant = "selected-qa-status-token"
	calls := 0
	client := &http.Client{Transport: ordinaryDevinStatusTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Basic "+grant+"-"+grant || r.URL.Path != DevinGetUserStatusPath {
			t.Fatal("status request changed selected authorization or destination")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		expected := BuildGetUserStatusRequest(grant)
		if string(body) != string(expected) {
			t.Fatal("status request altered ordinary serialized contents")
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("provider's private refusal detail"))}, nil
	})}
	service := NewDevinAuthService(client)
	status, err := service.FetchUserStatus(t.Context(), grant)
	var refusal interface{ StatusCode() int }
	if status != nil || !errors.As(err, &refusal) || refusal.StatusCode() != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("expected one explicit refusal, calls=%d, err=%v", calls, err)
	}
	if strings.Contains(err.Error(), "private refusal detail") {
		t.Fatal("raw provider refusal detail leaked")
	}
}

func TestOrdinaryDevinOAuthRecordKeepsGrantedSessionWhenStatusUnavailable(t *testing.T) {
	const grant = "qa-issued-devin-oauth-grant"
	var profile, status int
	fake := ordinaryDevinStatusTransport(func(req *http.Request) (*http.Response, error) {
		reply := &http.Response{Header: make(http.Header)}
		switch req.URL.Path {
		case "/v3/self":
			profile++
			if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer "+grant {
				t.Fatal("profile lookup changed its authorized context")
			}
			reply.StatusCode = http.StatusOK
			reply.Body = io.NopCloser(strings.NewReader(`{"user_name":"fixture","user_id":"fixture-user","org_id":"fixture-org"}`))
		case DevinGetUserStatusPath:
			status++
			data, err := io.ReadAll(req.Body)
			if err != nil || req.Header.Get("Authorization") != "Basic "+grant+"-"+grant || string(data) != string(BuildGetUserStatusRequest(grant)) {
				t.Fatal("status lookup changed its authorized context or added client/device metadata")
			}
			reply.StatusCode = http.StatusForbidden
			reply.Body = io.NopCloser(strings.NewReader("fixture provider refusal"))
		default:
			t.Fatalf("unexpected service %s", req.URL.Path)
		}
		return reply, nil
	})
	svc := NewDevinAuthService(&http.Client{Transport: fake})
	record, err := svc.CreateAuthRecord(t.Context(), grant)
	if err != nil || record == nil || record.Attributes["session_token"] != grant ||
		record.ID != "devin-fixture.json" || profile != 1 || status != 1 || len(record.Quota.Signals) != 0 {
		t.Fatalf("failed status enrichment changed granted OAuth record or retried: profile=%d status=%d err=%v", profile, status, err)
	}
}
