package cmd

import (
	"log"
	bot "github.com/issue-snooze/snooze-bot"
)

// RunBot is a subcommand `snoozebot run` -- Starts the daemon
//
// Flags:
//
//	config: --config -c (default: "") The path to the config file (optional)
func RunBot(config string) {
	log.Println("Running snooze bot...")
	bot.RunDaemon()
}
