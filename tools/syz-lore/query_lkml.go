// Copyright 2023 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/syzkaller/dashboard/dashapi"
	"github.com/google/syzkaller/pkg/email"
	"github.com/google/syzkaller/pkg/email/lore"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/tool"
	"golang.org/x/sync/errgroup"
)

// The syz-lore tool can parse Lore archives and extract syzbot-related conversations from there.

var (
	flagArchives  = flag.String("archives", "", "path to the folder with git archives")
	flagEmails    = flag.String("emails", "", "comma-separated list of own emails")
	flagDomains   = flag.String("domains", "", "comma-separated list of own domains")
	flagDashboard = flag.String("dashboard", "https://syzkaller.appspot.com", "dashboard address")
	flagAPIClient = flag.String("client", "", "the name of the API client")
	flagAPIKey    = flag.String("key", "", "api key")
	flagVerbose   = flag.Bool("v", false, "print more debug info")
)

func main() {
	defer tool.Init()()
	if !osutil.IsDir(*flagArchives) {
		tool.Failf("the arhives parameter must be a valid directory")
	}
	emails := strings.Split(*flagEmails, ",")
	domains := strings.Split(*flagDomains, ",")

	dash, err := dashapi.New(*flagAPIClient, *flagDashboard, *flagAPIKey)
	if err != nil {
		tool.Failf("dashapi failed: %v", err)
	}
	threads := processArchives(*flagArchives, emails, domains)
	for i, thread := range threads {
		messages := []dashapi.DiscussionMessage{}
		for _, m := range thread.Messages {
			messages = append(messages, dashapi.DiscussionMessage{
				ID:       m.MessageID,
				External: !emailInList(emails, m.Author),
				Time:     m.Date,
			})
		}
		discType := dashapi.DiscussionReport
		if strings.Contains(thread.Subject, "PATCH") {
			discType = dashapi.DiscussionPatch
		}
		log.Printf("saving %d/%d", i+1, len(threads))
		err := dash.SaveDiscussion(&dashapi.SaveDiscussionReq{
			Discussion: &dashapi.Discussion{
				ID:       thread.MessageID,
				Source:   dashapi.DiscussionLore,
				Type:     discType,
				Subject:  thread.Subject,
				BugIDs:   thread.BugIDs,
				Messages: messages,
			},
		})
		if err != nil {
			tool.Failf("dashapi failed: %v", err)
		}
	}
}

func processArchives(dir string, emails, domains []string) []*lore.Thread {
	entries, err := os.ReadDir(dir)
	if err != nil {
		tool.Failf("failed to read directory: %v", err)
	}
	threads := runtime.NumCPU()
	messages := make(chan *lore.EmailReader, threads*2)
	wg := sync.WaitGroup{}
	g, _ := errgroup.WithContext(context.Background())

	// Generate per-email jobs.
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		log.Printf("reading %s", path)
		wg.Add(1)
		g.Go(func() error {
			defer wg.Done()
			return lore.ReadArchive(path, messages)
		})
	}

	// Set up some worker threads.
	var repoEmails []*email.Email
	var mu sync.Mutex
	for i := 0; i < threads; i++ {
		g.Go(func() error {
			for rawMsg := range messages {
				body, err := rawMsg.Extract()
				if err != nil {
					continue
				}
				msg, err := email.Parse(bytes.NewReader(body), emails, nil, domains)
				if err != nil {
					continue
				}
				// Keep memory consumption low.
				msg.Body = ""
				msg.Patch = ""

				mu.Lock()
				repoEmails = append(repoEmails, msg)
				mu.Unlock()
			}
			return nil
		})
	}

	// Once all jobs are generated, close the processing channel.
	wg.Wait()
	close(messages)
	if err := g.Wait(); err != nil {
		tool.Failf("%s", err)
	}

	list := lore.Threads(repoEmails)
	log.Printf("collected %d email threads", len(list))

	ret := []*lore.Thread{}
	for _, d := range list {
		if d.BugIDs == nil {
			continue
		}
		ret = append(ret, d)
		if *flagVerbose {
			log.Printf("discussion ID=%s BugID=%s Subject=%s Messages=%d",
				d.MessageID, d.BugIDs, d.Subject, len(d.Messages))
		}
	}
	log.Printf("%d threads are related to syzbot", len(ret))
	return ret
}

func emailInList(list []string, email string) bool {
	for _, str := range list {
		if str == email {
			return true
		}
	}
	return false
}
