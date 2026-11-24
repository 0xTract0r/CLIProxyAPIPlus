package devin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// DevinGetUserStatusPath is the Connect-RPC unary endpoint for seat management and quota.
	DevinGetUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// DevinUserStatus contains plan, quota, and account metadata returned by GetUserStatus.
type DevinUserStatus struct {
	Email                       string    `json:"email,omitempty"`
	UserName                    string    `json:"user_name,omitempty"`
	UserID                      string    `json:"user_id,omitempty"`
	TeamID                      string    `json:"team_id,omitempty"`
	OrgID                       string    `json:"org_id,omitempty"`
	OrgName                     string    `json:"org_name,omitempty"`
	Plan                        string    `json:"plan,omitempty"`
	DailyQuotaRemainingPercent  int64     `json:"daily_quota_remaining_percent"`
	WeeklyQuotaRemainingPercent int64     `json:"weekly_quota_remaining_percent"`
	DailyQuotaResetAt           time.Time `json:"daily_quota_reset_at,omitempty"`
	WeeklyQuotaResetAt          time.Time `json:"weekly_quota_reset_at,omitempty"`
	PlanStart                   time.Time `json:"plan_start,omitempty"`
	PlanEnd                     time.Time `json:"plan_end,omitempty"`
}

// BuildGetUserStatusRequest serializes a Connect-RPC GetUserStatus request
// with the selected grant and ordinary runtime metadata alone.
func BuildGetUserStatusRequest(sessionToken string) []byte {
	var metadata []byte
	metadata = protowire.AppendTag(metadata, 3, protowire.BytesType)
	metadata = protowire.AppendString(metadata, sessionToken)
	metadata = protowire.AppendTag(metadata, 4, protowire.BytesType)
	metadata = protowire.AppendString(metadata, "en")
	metadata = protowire.AppendTag(metadata, 5, protowire.BytesType)
	metadata = protowire.AppendString(metadata, runtime.GOOS)
	request := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(request, metadata)
}

// ParseGetUserStatusResponse decodes the binary protobuf returned by GetUserStatus.
func ParseGetUserStatusResponse(data []byte) (*DevinUserStatus, error) {
	if len(data) == 0 {
		return nil, errors.New("empty response data")
	}

	status := &DevinUserStatus{}
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return nil, protowire.ParseError(n)
		}
		rem = rem[n:]

		if num == 1 && wireType == protowire.BytesType {
			// Field 1: user_status
			userStatusBytes, m := protowire.ConsumeBytes(rem)
			if m <= 0 {
				return nil, protowire.ParseError(m)
			}
			rem = rem[m:]

			parseUserStatus(userStatusBytes, status)
		} else {
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return nil, protowire.ParseError(m)
			}
			rem = rem[m:]
		}
	}

	return status, nil
}

func parseUserStatus(data []byte, status *DevinUserStatus) {
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return
		}
		rem = rem[n:]

		if wireType == protowire.BytesType {
			bytesVal, m := protowire.ConsumeBytes(rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]

			switch num {
			case 3:
				status.UserName = string(bytesVal)
			case 5:
				status.TeamID = string(bytesVal)
			case 7:
				status.Email = string(bytesVal)
			case 13:
				parsePlanStatus(bytesVal, status)
			case 36:
				status.UserID = string(bytesVal)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]
		}
	}
}

func parsePlanStatus(data []byte, status *DevinUserStatus) {
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return
		}
		rem = rem[n:]

		switch wireType {
		case protowire.BytesType:
			bytesVal, m := protowire.ConsumeBytes(rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]

			switch num {
			case 1:
				parsePlanInfo(bytesVal, status)
			case 2:
				sec := parseSecondsSubfield(bytesVal)
				if sec > 0 {
					status.PlanStart = time.Unix(sec, 0).UTC()
				}
			case 3:
				sec := parseSecondsSubfield(bytesVal)
				if sec > 0 {
					status.PlanEnd = time.Unix(sec, 0).UTC()
				}
			}

		case protowire.VarintType:
			val, m := protowire.ConsumeVarint(rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]

			switch num {
			case 14:
				status.DailyQuotaRemainingPercent = int64(val)
			case 15:
				status.WeeklyQuotaRemainingPercent = int64(val)
			case 17:
				if val > 0 {
					status.DailyQuotaResetAt = time.Unix(int64(val), 0).UTC()
				}
			case 18:
				if val > 0 {
					status.WeeklyQuotaResetAt = time.Unix(int64(val), 0).UTC()
				}
			}

		default:
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]
		}
	}
}

func parsePlanInfo(data []byte, status *DevinUserStatus) {
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return
		}
		rem = rem[n:]

		if wireType == protowire.BytesType {
			bytesVal, m := protowire.ConsumeBytes(rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]

			switch num {
			case 2:
				status.Plan = string(bytesVal)
			case 33:
				parsePlanInfoOrg(bytesVal, status)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]
		}
	}
}

func parsePlanInfoOrg(data []byte, status *DevinUserStatus) {
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return
		}
		rem = rem[n:]

		if wireType == protowire.BytesType {
			bytesVal, m := protowire.ConsumeBytes(rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]

			switch num {
			case 4:
				status.OrgID = string(bytesVal)
			case 8:
				status.OrgName = string(bytesVal)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return
			}
			rem = rem[m:]
		}
	}
}

func parseSecondsSubfield(data []byte) int64 {
	rem := data
	for len(rem) > 0 {
		num, wireType, n := protowire.ConsumeTag(rem)
		if n <= 0 {
			return 0
		}
		rem = rem[n:]

		if wireType == protowire.VarintType {
			val, m := protowire.ConsumeVarint(rem)
			if m <= 0 {
				return 0
			}
			rem = rem[m:]
			if num == 1 {
				return int64(val)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, wireType, rem)
			if m <= 0 {
				return 0
			}
			rem = rem[m:]
		}
	}
	return 0
}

// FetchUserStatus queries Connect-RPC endpoint GetUserStatus for quota, plan, and user details.
func (s *DevinAuthService) FetchUserStatus(ctx context.Context, sessionToken string) (*DevinUserStatus, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("devin auth service: uninitialized client")
	}

	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return nil, errors.New("devin auth service: session token is required")
	}

	reqBody := BuildGetUserStatusRequest(sessionToken)

	endpoint := fmt.Sprintf("%s%s", strings.TrimRight(s.serverBaseURL, "/"), DevinGetUserStatusPath)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(reqBody)))
	if errReq != nil {
		return nil, errReq
	}

	req.Header.Set("Authorization", fmt.Sprintf("Basic %s-%s", sessionToken, sessionToken))
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Accept", "*/*")
	req.Header["User-Agent"] = []string{""}

	resp, errDo := s.client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, devinUserStatusError{status: resp.StatusCode}
	}

	respBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, errRead
	}

	return ParseGetUserStatusResponse(respBytes)
}

type devinUserStatusError struct{ status int }

func (e devinUserStatusError) StatusCode() int { return e.status }

func (e devinUserStatusError) Error() string {
	return fmt.Sprintf("Devin user status unavailable (HTTP %d)", e.status)
}
