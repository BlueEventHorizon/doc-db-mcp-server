package store

import "time"

// timeNowRFC3339 は現在時刻を RFC3339 形式で返す。
func timeNowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
