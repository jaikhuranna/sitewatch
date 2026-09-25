package main

import (
	"reflect"
	"testing"
)

const listingPage = `<html><head><base href="/bikes/"></head><body>
<a href="listing/1?utm_source=x"><div><p class="name">KTM Duke 390</p><p class="info">2,415 KMs • 2023</p>
  <p class="price">₹2,98,000</p></div></a>
<div class="card"><p class="name">Honda  Activa 125</p><p class="price">₹92,000</p><a href="/bikes/listing/2">view</a></div>
<div class="card"><p class="name"> </p><a href="/bikes/listing/3">view</a></div>
</body></html>`

func TestPageItems(t *testing.T) {
	got, err := pageItems(listingPage, "https://example.com/search?city=blr",
		&ItemsSpec{Title: "p.name", Detail: "p.info, p.price"})
	if err != nil {
		t.Fatal(err)
	}
	want := []item{
		{"KTM Duke 390", "2,415 KMs • 2023 · ₹2,98,000", "https://example.com/bikes/listing/1"},
		{"Honda Activa 125", "₹92,000", "https://example.com/bikes/listing/2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	w, _ := Watcher{Name: "x", URL: "u", Items: &ItemsSpec{Title: "p"}}.expand(Filter{Include: []string{"duke", "\\br15(\\b|m)"}})
	lines, err := filterItems(append(got, item{"Yamaha R15 V4", "", "l"}, item{"Hero R150", "", "l"}), w)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "KTM Duke 390 | 2,415 KMs • 2023 · ₹2,98,000 | https://example.com/bikes/listing/1" {
		t.Errorf("filtered lines: %q", lines)
	}
}
