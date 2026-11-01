// Package listkey is the one rule a list keys its items by (D67): the core's ListState, which
// diffs a list, and the Go client, which applies the diffs, both key with ItemID, as the
// TypeScript runtime's itemID does in the browser.
package listkey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ItemID is the key a list keys an item by (ListState, and every client applying a
// ListChange the same way; its errors: no object, no or a null id, an id neither string nor
// number), read from the member spelled exactly "id"
// (no case-insensitive alias): a JSON string as it is, a JSON number as JavaScript's Number
// toString writes it (1, 1.0 and 1e0 are one id; 1234567 stays plain; 1e21 and 1e-7 take
// exponent form; -0 is 0), so the server and the browser key the same.
func ItemID(item json.RawMessage) (string, error) {
	var members map[string]json.RawMessage
	err := json.Unmarshal(item, &members)
	rawID, present := members["id"]
	missing := err != nil || !present || bytes.Equal(rawID, []byte("null"))
	if missing {
		return "", fmt.Errorf("livewire: a list item has no id: %s", item)
	}
	var text string
	if err := json.Unmarshal(rawID, &text); err == nil {
		return text, nil
	}
	var number float64
	if err := json.Unmarshal(rawID, &number); err == nil {
		return formatNumberID(number), nil
	}
	return "", fmt.Errorf("livewire: a list item's id is neither a string nor a number: %s", rawID)
}

// formatNumberID writes a number the way JavaScript's Number toString does, so a numeric id
// keys the same on the server and in the browser: plain digits for 1e-6 <= |n| < 1e21,
// exponent form outside, and -0 as 0.
func formatNumberID(number float64) string {
	if number == 0 {
		return "0"
	}
	magnitude := math.Abs(number)
	if magnitude >= 1e-6 && magnitude < 1e21 {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(number, 'e', -1, 64), "e")
	sign, digits := exponent[:1], strings.TrimLeft(exponent[1:], "0")
	return mantissa + "e" + sign + digits
}
