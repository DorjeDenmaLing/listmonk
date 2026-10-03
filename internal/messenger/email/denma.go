package email

// denma: BeforePush, if set, is called before every message any Emailer
// sends, and may block (for a send limit shared by every app in the process,
// cmd/denma_sending.go). Campaign messages, notifications, opt-ins and
// password resets all pass here.
var BeforePush func()
