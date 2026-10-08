package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"vellum/internal/config"
	"vellum/internal/db"
)

// cmdChat manages saved chatbot sessions. The chats themselves live in the
// web UI (streaming); the CLI covers listing, creating, renaming and
// deleting them.
//
//	vellum chat list
//	vellum chat show ID
//	vellum chat new --library | --document ID | --tag T | --category C | --collection ID
//	vellum chat rename ID TITLE
//	vellum chat delete ID
func cmdChat(cfg *config.Config, args []string) {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	conn := mustOpen(cfg)
	switch sub {
	case "list":
		sessions, err := db.ListChatSessions(conn)
		if err != nil {
			log.Fatalf("chat: %s", err)
		}
		if jsonOut {
			printJSON(sessions)
			return
		}
		if len(sessions) == 0 {
			fmt.Println("no chat sessions yet")
			return
		}
		for _, s := range sessions {
			fmt.Printf("#%d  %-40s  %s %s  (%d msg, updated %s)\n",
				s.ID, clip(s.Title, 40), s.ScopeKind, s.ScopeValue,
				s.Messages, s.UpdatedAt)
		}
	case "show":
		if len(args) != 1 {
			log.Fatalf("usage: vellum chat show ID")
		}
		id := mustInt64(args[0], "chat id")
		sess, err := db.GetChatSession(conn, id)
		if err != nil {
			log.Fatalf("chat: %s", err)
		}
		msgs, err := db.ListChatMessages(conn, id)
		if err != nil {
			log.Fatalf("chat: %s", err)
		}
		if jsonOut {
			printJSON(map[string]any{"session": sess, "messages": msgs})
			return
		}
		fmt.Printf("#%d %s — scope %s %s\n", sess.ID, sess.Title,
			sess.ScopeKind, sess.ScopeValue)
		for _, m := range msgs {
			fmt.Printf("\n[%s] %s\n", m.Role, m.Content)
			if m.ToolLog != "" {
				fmt.Printf("  tools: %s\n", m.ToolLog)
			}
		}
	case "new":
		fs := flag.NewFlagSet("chat new", flag.ExitOnError)
		library := fs.Bool("library", false, "whole-library scope")
		document := fs.Int64("document", 0, "document id scope")
		tag := fs.String("tag", "", "tag scope")
		category := fs.String("category", "", "category (shelf) scope")
		collection := fs.Int64("collection", 0, "collection id scope")
		title := fs.String("title", "", "session title")
		fs.Parse(args)
		kind, value := "", ""
		switch {
		case *library:
			kind = "library"
		case *document > 0:
			kind, value = "document", fmt.Sprint(*document)
		case *tag != "":
			kind, value = "tag", *tag
		case *category != "":
			kind, value = "category", *category
		case *collection > 0:
			kind, value = "collection", fmt.Sprint(*collection)
		default:
			log.Fatalf("chat new needs a scope: --library, --document ID, --tag T, --category C or --collection ID")
		}
		sess, err := db.CreateChatSession(conn, *title, kind, value)
		if err != nil {
			log.Fatalf("chat: %s", err)
		}
		if jsonOut {
			printJSON(sess)
			return
		}
		fmt.Printf("created chat #%d (%s %s)\n", sess.ID, kind, value)
	case "rename":
		if len(args) < 2 {
			log.Fatalf("usage: vellum chat rename ID TITLE")
		}
		id := mustInt64(args[0], "chat id")
		sess, err := db.RenameChatSession(conn, id, strings.Join(args[1:], " "))
		if err != nil {
			log.Fatalf("chat: %s", err)
		}
		fmt.Printf("#%d renamed to %q\n", sess.ID, sess.Title)
	case "delete", "rm":
		for _, a := range args {
			id := mustInt64(a, "chat id")
			if err := db.DeleteChatSession(conn, id); err != nil {
				log.Fatalf("chat: %s", err)
			}
			fmt.Printf("deleted chat #%d\n", id)
		}
	default:
		log.Fatalf("unknown chat subcommand: %s", sub)
	}
}

func mustInt64(s, what string) int64 {
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil || n <= 0 {
		log.Fatalf("%s must be a positive integer", what)
	}
	return n
}
