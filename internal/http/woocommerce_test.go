package http

import "testing"

func TestWoocommerceBaseURL(t *testing.T) {
	cases := map[string]string{
		"store.com":                       "https://store.com",
		"  store.com/  ":                  "https://store.com",
		"https://store.com":               "https://store.com",
		"https://store.com/":              "https://store.com",
		"http://store.com":                "http://store.com",
		"https://store.com/shop":          "https://store.com/shop",
		"https://store.com/wp-json/wc/v3": "https://store.com",
		"https://user:pass@store.com":     "https://store.com", // userinfo stripped
		"":                                "",
		"   ":                             "",
		"${secrets.URL}":                  "",
		"ftp://store.com":                 "",
	}
	for in, want := range cases {
		if got := woocommerceBaseURL(in); got != want {
			t.Errorf("woocommerceBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWoocommerceEvents(t *testing.T) {
	// Empty → all 12 topics.
	if got := woocommerceEvents(""); len(got) != len(woocommerceAllEvents) {
		t.Errorf("empty selection should default to all %d topics, got %d", len(woocommerceAllEvents), len(got))
	}
	// JSON-array form.
	got := woocommerceEvents(`["order.created","product.updated"]`)
	if len(got) != 2 || got[0] != "order.created" || got[1] != "product.updated" {
		t.Errorf("JSON-array parse wrong: %v", got)
	}
	// CSV form with spaces.
	got = woocommerceEvents("order.created, coupon.created ")
	if len(got) != 2 || got[0] != "order.created" || got[1] != "coupon.created" {
		t.Errorf("CSV parse wrong: %v", got)
	}
}

func TestWoocommerceSameEventSet(t *testing.T) {
	if !woocommerceSameEventSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order-independent equal sets should match")
	}
	if woocommerceSameEventSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("different-length sets should not match")
	}
	if woocommerceSameEventSet([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("different members should not match")
	}
}
