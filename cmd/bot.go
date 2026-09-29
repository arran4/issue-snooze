package cmd

import (
	bot "github.com/arran4/issue-snooze"
	"log"
)

// RunBot is a subcommand `issue-snooze run` -- Starts the daemon
//
// Flags:
//
//	config: --config -c (default: "") The path to the config file (optional)
func RunBot(config string) {
	log.Println("Running snooze bot...")
	bot.RunDaemon()
}
