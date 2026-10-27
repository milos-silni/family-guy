package main

import (
	"reflect"
	"testing"
)

func TestParseItemLines(t *testing.T) {
	// Pasted from a notes app: bullets wrapped in invisible word joiners (U+2060).
	text := "•⁠  ⁠[ ] trulex krpe\n" +
		"•⁠  ⁠[ ] Mila jogurt 4 kom \n" +
		"\n" +
		"- [x] mleko 2L\n" +
		"* hleb\n" +
		"sir 0,5 kg\n" +
		"bio grozdje i jos neko voce ako ima osim jabuka i kruske\n"

	got := parseItemLines(text)
	want := []Favorite{
		{Name: "trulex krpe"},
		{Name: "Mila jogurt", Quantity: "4 kom"},
		{Name: "mleko", Quantity: "2L"},
		{Name: "hleb"},
		{Name: "sir", Quantity: "0,5 kg"},
		{Name: "bio grozdje i jos neko voce ako ima osim jabuka i kruske"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseItemLines:\n got %+v\nwant %+v", got, want)
	}
}
