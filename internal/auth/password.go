package auth

import "golang.org/x/crypto/bcrypt"

// dummyHash is a valid bcrypt hash of a random string. It is compared against
// when no account matches, so an attacker cannot distinguish "no such user"
// from "wrong password" by timing (user-enumeration defence).
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// DummyCompare performs a bcrypt comparison against a fixed hash to burn roughly
// the same time a real check would, keeping login timing constant.
func DummyCompare(pw string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(pw))
}
