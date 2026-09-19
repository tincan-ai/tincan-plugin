package core

type EncryptionRoomPolicy struct {
	Members  map[string][]string `json:"members"`
	Channels map[string]string   `json:"channels"`
}
