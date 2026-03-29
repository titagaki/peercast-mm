package id

import (
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"
)

// TestChannelID_Deterministic は同一入力が常に同一 ID を返すことを確認する。
func TestChannelID_Deterministic(t *testing.T) {
	bid := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	a := ChannelID(bid, "TestChannel", "Pop", 128)
	b := ChannelID(bid, "TestChannel", "Pop", 128)
	if a != b {
		t.Errorf("non-deterministic: %v != %v", a, b)
	}
}

// TestChannelID_ZeroInputs は全ゼロ入力が全ゼロ ID を返すことを確認する。
// broadcastID=zeros, name="", genre="", bitrate=0 ではステップ1は変化なし、
// ステップ2は空文字列のためXORが行われず、結果はそのままゼロ。
func TestChannelID_ZeroInputs(t *testing.T) {
	var bid pcp.GnuID
	got := ChannelID(bid, "", "", 0)
	var want pcp.GnuID
	if got != want {
		t.Errorf("got %v, want all-zero", got)
	}
}

// TestChannelID_BitrateXOR はビットレートが全バイトに XOR されることを確認する。
// broadcastID=zeros, name="", genre="", bitrate=255 の場合、
// ステップ1で全バイトが 0xFF になり、ステップ2は空文字列のため変化なし。
func TestChannelID_BitrateXOR(t *testing.T) {
	var bid pcp.GnuID
	got := ChannelID(bid, "", "", 255)
	var want pcp.GnuID
	for i := range want {
		want[i] = 0xFF
	}
	if got != want {
		t.Errorf("got %v, want all-0xFF", got)
	}
}

// TestChannelID_BitrateUsesLower8Bits は bitrate の下位8ビットのみが使われることを確認する。
// bitrate=256 は byte(256)=0 となり、bitrate=0 と同じ結果になる。
func TestChannelID_BitrateUsesLower8Bits(t *testing.T) {
	bid := pcp.GnuID{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22,
		0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}
	a := ChannelID(bid, "ch", "genre", 0)
	b := ChannelID(bid, "ch", "genre", 256) // byte(256)==0
	if a != b {
		t.Errorf("bitrate=0 and bitrate=256 should produce same ID")
	}
}

// TestChannelID_DifferentNamesDifferentIDs は異なるチャンネル名が異なる ID を生成することを確認する。
func TestChannelID_DifferentNamesDifferentIDs(t *testing.T) {
	bid := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	a := ChannelID(bid, "Channel A", "Pop", 0)
	b := ChannelID(bid, "Channel B", "Pop", 0)
	if a == b {
		t.Errorf("different names should produce different IDs")
	}
}

// TestChannelID_DifferentBroadcastIDsDifferentIDs は異なる BroadcastID が異なる ID を生成することを確認する。
func TestChannelID_DifferentBroadcastIDsDifferentIDs(t *testing.T) {
	bidA := pcp.GnuID{1}
	bidB := pcp.GnuID{2}
	a := ChannelID(bidA, "ch", "genre", 64)
	b := ChannelID(bidB, "ch", "genre", 64)
	if a == b {
		t.Errorf("different broadcastIDs should produce different IDs")
	}
}

// TestChannelID_GoldenValue は手計算した期待値と一致することを確認する。
//
// broadcastID = [1..16], name="AB", genre="X", bitrate=0 の場合:
//
//	step1: bitrate=0 なので変化なし
//	step2: maxLen=max(2,1)=2, n=(2/16+1)*16=16 回ループ
//	       "AB" と "X" を循環させながら XOR
//
// 各バイトの計算:
//
//	i=0:  1  ^ 'A'^'X' = 1^65^88 = 24
//	i=1:  2  ^ 'B'     = 2^66    = 64   (genre 末尾リセット、その回はXORなし)
//	i=2:  3  ^ 'X'     = 3^88    = 91   (name 末尾リセット、その回はXORなし)
//	i=3:  4  ^ 'A'     = 4^65    = 69   (genre リセット済み、その回はXORなし)
//	i=4:  5  ^ 'B'^'X' = 5^66^88 = 31
//	i=5:  6            = 6       (name/genre 両方リセット、その回はXORなし)
//	i=6:  7  ^ 'A'^'X' = 7^65^88 = 30
//	i=7:  8  ^ 'B'     = 8^66    = 74
//	i=8:  9  ^ 'X'     = 9^88    = 81
//	i=9:  10 ^ 'A'     = 10^65   = 75
//	i=10: 11 ^ 'B'^'X' = 11^66^88 = 17
//	i=11: 12           = 12
//	i=12: 13 ^ 'A'^'X' = 13^65^88 = 20
//	i=13: 14 ^ 'B'     = 14^66   = 76
//	i=14: 15 ^ 'X'     = 15^88   = 87
//	i=15: 16 ^ 'A'     = 16^65   = 81
func TestChannelID_GoldenValue(t *testing.T) {
	bid := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	want := pcp.GnuID{24, 64, 91, 69, 31, 6, 30, 74, 81, 75, 17, 12, 20, 76, 87, 81}
	got := ChannelID(bid, "AB", "X", 0)
	if got != want {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestChannelID_DoesNotMutateBroadcastID は入力の BroadcastID が変化しないことを確認する。
func TestChannelID_DoesNotMutateBroadcastID(t *testing.T) {
	orig := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	bid := orig
	ChannelID(bid, "ch", "genre", 128)
	if bid != orig {
		t.Errorf("ChannelID mutated broadcastID: got %v, want %v", bid, orig)
	}
}
