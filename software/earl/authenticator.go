package main

// TODO
// - add reloadIfChanged(): check file-timestamp and reload if needed
// - We need the concept of an 'open space'. If the space is open (e.g.
//   two members state that they are there), then regular users should come
//   in independent of time.

import (
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

type Authenticator interface {
	// Given a code, is the user allowed to access "target" ?
	AuthUser(code string, target Target) (ok bool, msg string)

	// Given the authenticator token (checked for memberness),
	// add the given user.
	// Updates the file
	AddNewUser(authentication_code string, user User) (ok bool, msg string)

	// Find a user for the given string. Returns a copy or 'nil' if the
	// user doesn't exist.
	FindUser(plain_code string) *User
}

type FileBasedAuthenticator struct {
	userFilename  string
	fileTimestamp time.Time // Timestamp at read time.

	// Map of codes to users. Quick way to look-up auth. Never use directly,
	// use findUserSynchronized() and addUserSynchronized() for locking.
	validUsers     map[string]*User
	validUsersLock sync.Mutex

	clock Clock // Our source of time. Useful for simulated clock in tests
}

func NewFileBasedAuthenticator(userFilename string) *FileBasedAuthenticator {
	a := &FileBasedAuthenticator{
		userFilename: userFilename,
		validUsers:   make(map[string]*User),
		clock:        RealClock{},
	}

	if !a.readUserFile() {
		return nil
	} else {
		return a
	}
}

// We hash the authentication codes, as we don't need/want knowledge
// of actual IDs just to be able to verify.
//
// Note, this hash can _not_ protect against brute-force attacks; if you
// have access to the file, some CPU cycles and can emulate tokens, you are in
// (pin-codes are relatively short, and some older Mifare cards only have
// 32Bit IDs, so no protection against cheaply generated rainbow tables).
// But then again, you are more than welcome in a Hackerspace in that case :)
//
// So we merely protect against accidentally revealing a PIN or card-ID and
// their lengths while browsing the file.
func hashAuthCode(plain string) string {
	hashgen := md5.New()
	io.WriteString(hashgen, "MakeThisALittleBitLongerToChewOnEarlFoo"+plain)
	return hex.EncodeToString(hashgen.Sum(nil))
}

// Verify that code is long enough (and probably other syntactical things, such
// as not all the same digits and such)
func hasMinimalCodeRequirements(code string) bool {
	// 32Bit Mifare are 8 characters hex, this is to impose a minimum
	// 'strength' of a pin.
	return len(code) >= 6
}

// Find user. Synchronizes map.
func (a *FileBasedAuthenticator) findUserSynchronized(plain_code string) *User {
	a.validUsersLock.Lock()
	defer a.validUsersLock.Unlock()
	user, _ := a.validUsers[hashAuthCode(plain_code)]
	return user
}

// Add user to the internal data structure.
// Makes sure the data structure is synchronized.
func (a *FileBasedAuthenticator) addUserSynchronized(user *User) bool {
	a.validUsersLock.Lock()
	defer a.validUsersLock.Unlock()
	// First verify that there is no code in there that is already set..
	for _, code := range user.Codes {
		if a.validUsers[code] != nil {
			log.Printf("Ignoring multiple used code '%s'", code)
			return false // Existing user with that code
		}
	}
	// Then ok to add.
	for _, code := range user.Codes {
		a.validUsers[code] = user
	}
	return true
}

//
// Read the user CSV file
//
// It is name, level, code[,code...]
func (a *FileBasedAuthenticator) readUserFile() bool {
	if a.userFilename == "" {
		log.Println("RFID-user file not provided")
		return false
	}
	f, err := os.Open(a.userFilename)
	if err != nil {
		log.Println("Could not read RFID user-file", err)
		return false
	}

	fileinfo, _ := os.Stat(a.userFilename)
	a.fileTimestamp = fileinfo.ModTime()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 //variable length fields

	counts := make(map[Level]int)
	total := 0
	log.Printf("Reading %s", a.userFilename)
	for {
		user, done := NewUserFromCSV(reader)
		if done {
			break
		}
		if user == nil {
			continue // e.g. due to comment or short line
		}
		a.addUserSynchronized(user)
		counts[user.UserLevel]++
		total++
	}
	log.Printf("Read %d users from %s", total, a.userFilename)
	for level, count := range counts {
		log.Printf("%13s %4d", level, count)
	}
	return true
}

func (a *FileBasedAuthenticator) reloadIfChanged() {
	fileinfo, err := os.Stat(a.userFilename)
	if err != nil {
		return // well, ok then.
	}
	if a.fileTimestamp == fileinfo.ModTime() {
		return // nothing to do.
	}
	log.Printf("Refreshing changed %s (%s -> %s)\n",
		a.userFilename,
		a.fileTimestamp.Format("2006-01-02 15:04:05"),
		fileinfo.ModTime().Format("2006-01-02 15:04:05"))

	// For now, we are doing it simple: just create
	// a new authenticator and steal the result.
	// If we allow to modify users in-memory, we need to make
	// sure that we don't replace contents while that is happening.
	newAuth := NewFileBasedAuthenticator(a.userFilename)
	if newAuth == nil {
		return
	}
	a.validUsersLock.Lock()
	defer a.validUsersLock.Unlock()
	a.fileTimestamp = newAuth.fileTimestamp
	a.validUsers = newAuth.validUsers
}

func (a *FileBasedAuthenticator) FindUser(plain_code string) *User {
	user := a.findUserSynchronized(plain_code)
	if user == nil {
		return nil
	}
	retval := *user // Copy, so that caller does not mess with state.
	// TODO: stash away the original pointer in the copy, which we then
	// use for update operation later. Once we have UpdateUser()
	return &retval
}

// TODO: return readable error instead of false.
func (a *FileBasedAuthenticator) AddNewUser(authentication_code string, user User) (bool, string) {
	// Only members can add.
	authMember := a.findUserSynchronized(authentication_code)
	if authMember == nil {
		return false, "Couldn't find member with authentication code"
	}
	if authMember.UserLevel != LevelMember {
		return false, "Non-member AddNewUser attempt"
	}
	if !authMember.InValidityPeriod(a.clock.Now()) {
		return false, "Auth-Member not in valid time-frame"
	}

	// TODO: Verify that there is some identifying information for the
	// user, otherwise only allow limited time range.

	// Right now, one sponsor, in the future we might require
	// a list depending on short/long-term expiry.
	user.Sponsors = []string{hashAuthCode(authentication_code)}
	// If no valid from date is given, then this is creation time.
	if user.ValidFrom.IsZero() {
		user.ValidFrom = a.clock.Now()
	}
	// Are the codes used unique ?
	if !a.addUserSynchronized(&user) {
		return false, "Duplicate codes while adding user"
	}

	// Just append the user to the file which is sufficient for AddNewUser()
	// TODO: When we allow for updates, we need to dump out the whole file
	// and do atomic rename.
	f, err := os.OpenFile(a.userFilename, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return false, err.Error()
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	user.WriteCSV(writer)
	writer.Flush()

	fileinfo, _ := os.Stat(a.userFilename)
	a.fileTimestamp = fileinfo.ModTime()

	return true, ""
}

// Check if access for a given code is granted to a given Target
func (a *FileBasedAuthenticator) AuthUser(code string, target Target) (bool, string) {
	if !hasMinimalCodeRequirements(code) {
		return false, "Auth failed: too short code."
	}
	a.reloadIfChanged()
	user := a.findUserSynchronized(code)
	if user == nil {
		return false, "No user for code"
	}
	// In case of Hiatus users, be a bit more specific with logging: this
	// might be someone stolen a token of some person on leave or attempt
	// of a blocked user to get access.
	if user.UserLevel == LevelHiatus {
		return false, fmt.Sprintf("User on hiatus '%s <%s>'", user.Name, user.ContactInfo)
	}
	if !user.InValidityPeriod(a.clock.Now()) {
		return false, "Code not valid yet/expired"
	}
	return a.levelHasAccess(user.UserLevel, target)
}

// Certain levels only have access during the daytime
// This implements that logic, which is 11:00 - 21:59
func (a *FileBasedAuthenticator) isUserDaytime() bool {
	hour := a.clock.Now().Hour()
	return hour >= 11 && hour < 22 // 11:00..21:59
}
func (a *FileBasedAuthenticator) isFulltimeUserDaytime() bool {
	hour := a.clock.Now().Hour()
	return hour >= 7 && hour <= 23 // 7:00..23:59
}

func (a *FileBasedAuthenticator) levelHasAccess(level Level, target Target) (bool, string) {
	switch level {
	case LevelMember:
		return true, "" // Members always have access.

	case LevelFulltimeUser:
		isday := a.isFulltimeUserDaytime()
		if !isday {
			return false, "Fulltime user outside daytime"
		}
		return isday, ""

	case LevelUser:
		// TODO: we might want to make this dependent simply on
		// members having 'opened' the space.
		isday := a.isUserDaytime()
		if !isday {
			return false, "Regular user outside daytime"
		}
		return isday, ""

	case LevelHiatus:
		return false, "On Hiatus"

	case LevelLegacy: // TODO: consider if we still need this level.
		isday := a.isUserDaytime()
		if !isday {
			return false, "Gate user outside daytime"
		}
		return target == TargetDownstairs, ""
	}
	return false, ""
}
