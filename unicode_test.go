package main

import "testing"

func TestEastAsianChar(t *testing.T) {
	lineCol := 1
	screenCol := columnToScreenWidth([]rune("世界"), lineCol)
	if want := 2; screenCol != want {
		t.Fatalf("want %d, got %d", want, screenCol)
	}
	if col := columnFromScreenWidth([]rune("世界"), screenCol); col != lineCol {
		t.Fatalf("want %d, got %d", lineCol, col)
	}
}
