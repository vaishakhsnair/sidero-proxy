package ban

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sys/unix"
)

// Watcher watches Pterodactyl volume directories for banned-ips.json changes.
type Watcher struct {
	volumesRoot string
	rdb         *redis.Client
	// per-uuid last-seen banned-ips entries (ip → entry)
	snapshots map[string]map[string]bannedEntry
}

type bannedEntry struct {
	IP      string `json:"ip"`
	Source  string `json:"source"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
	Created string `json:"created"`
}

func NewWatcher(volumesRoot string, rdb *redis.Client) *Watcher {
	return &Watcher{
		volumesRoot: volumesRoot,
		rdb:         rdb,
		snapshots:   make(map[string]map[string]bannedEntry),
	}
}

// Start watches the volumes root and all banned-ips.json files within.
func (w *Watcher) Start(ctx context.Context) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify_init: %w", err)
	}
	defer unix.Close(fd)

	// wd → path mapping
	wds := make(map[int]string)

	// Watch the volumes root for new UUID directories
	rootWD, err := unix.InotifyAddWatch(fd, w.volumesRoot, unix.IN_CREATE)
	if err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", w.volumesRoot, err)
	}
	wds[rootWD] = w.volumesRoot

	// Discover existing UUIDs and watch their banned-ips.json
	entries, err := os.ReadDir(w.volumesRoot)
	if err != nil {
		return fmt.Errorf("ReadDir %s: %w", w.volumesRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uuid := entry.Name()
		banFile := filepath.Join(w.volumesRoot, uuid, "banned-ips.json")
		if _, err := os.Stat(banFile); err != nil {
			continue
		}
		dir := filepath.Dir(banFile)
		wd, err := unix.InotifyAddWatch(fd, dir, unix.IN_CLOSE_WRITE)
		if err != nil {
			log.Printf("warn: inotify_add_watch %s: %v", dir, err)
			continue
		}
		wds[wd] = uuid
		// Load initial snapshot
		w.loadSnapshot(ctx, uuid)
	}

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, err := unix.Read(fd, buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("inotify read: %w", err)
		}

		offset := 0
		for offset < n {
			if offset+unix.SizeofInotifyEvent > n {
				break
			}
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			wd := int(event.Wd)
			mask := event.Mask
			nameLen := event.Len

			var name string
			if nameLen > 0 {
				nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+int(nameLen)]
				name = strings.TrimRight(string(nameBytes), "\x00")
			}
			offset += unix.SizeofInotifyEvent + int(nameLen)

			path, ok := wds[wd]
			if !ok {
				continue
			}

			if path == w.volumesRoot && mask&unix.IN_CREATE != 0 {
				// New UUID directory appeared
				uuid := name
				dirPath := filepath.Join(w.volumesRoot, uuid)
				banFile := filepath.Join(dirPath, "banned-ips.json")
				// Wait briefly for file to be created
				go func() {
					for i := 0; i < 10; i++ {
						time.Sleep(500 * time.Millisecond)
						if _, err := os.Stat(banFile); err == nil {
							break
						}
					}
					watchWD, err := unix.InotifyAddWatch(fd, dirPath, unix.IN_CLOSE_WRITE)
					if err != nil {
						log.Printf("warn: inotify_add_watch %s: %v", dirPath, err)
						return
					}
					wds[watchWD] = uuid
					w.loadSnapshot(ctx, uuid)
				}()
			} else if mask&unix.IN_CLOSE_WRITE != 0 && name == "banned-ips.json" {
				// banned-ips.json changed in this UUID's directory
				uuid := path
				go w.handleBannedIPsChange(ctx, uuid)
			}
		}
	}
}

func (w *Watcher) loadSnapshot(ctx context.Context, uuid string) {
	entries, err := readBannedIPs(filepath.Join(w.volumesRoot, uuid, "banned-ips.json"))
	if err != nil {
		return
	}
	snap := make(map[string]bannedEntry, len(entries))
	for _, e := range entries {
		snap[e.IP] = e
	}
	w.snapshots[uuid] = snap
}

func (w *Watcher) handleBannedIPsChange(ctx context.Context, uuid string) {
	banFile := filepath.Join(w.volumesRoot, uuid, "banned-ips.json")
	entries, err := readBannedIPs(banFile)
	if err != nil {
		log.Printf("warn: read %s: %v", banFile, err)
		return
	}

	prev := w.snapshots[uuid]
	if prev == nil {
		prev = make(map[string]bannedEntry)
	}

	var newEntries []bannedEntry
	current := make(map[string]bannedEntry, len(entries))
	for _, e := range entries {
		current[e.IP] = e
		if _, seen := prev[e.IP]; !seen {
			newEntries = append(newEntries, e)
		}
	}

	for _, entry := range newEntries {
		w.processBan(ctx, uuid, entry)
	}

	w.snapshots[uuid] = current
}

func (w *Watcher) processBan(ctx context.Context, uuid string, entry bannedEntry) {
	internalIP := entry.IP

	// Resolve internal IP → real IP + proxy
	rmapVal, err := w.rdb.Get(ctx, "rmap:"+internalIP).Result()
	if err != nil {
		log.Printf("ban: rmap:%s not found: %v", internalIP, err)
		return
	}
	var rmap struct {
		RealIP string `json:"real_ip"`
		Proxy  string `json:"proxy"`
	}
	if err := json.Unmarshal([]byte(rmapVal), &rmap); err != nil {
		log.Printf("ban: unmarshal rmap: %v", err)
		return
	}
	realIP := rmap.RealIP

	// Get all internal IPs across all proxies for this real IP
	allMappings, err := w.rdb.HGetAll(ctx, "map:"+realIP).Result()
	if err != nil {
		log.Printf("ban: HGETALL map:%s: %v", realIP, err)
		return
	}

	// Store ban in Redis
	if err := w.rdb.SAdd(ctx, "banned:real", realIP).Err(); err != nil {
		log.Printf("ban: SADD banned:real: %v", err)
	}

	internalIPs := make(map[string]string)
	for proxy, intIP := range allMappings {
		if err := w.rdb.SAdd(ctx, "banned:internal:"+proxy, intIP).Err(); err != nil {
			log.Printf("ban: SADD banned:internal:%s: %v", proxy, err)
		}
		internalIPs[proxy] = intIP
	}

	// Publish ban event
	event := BanEvent{
		RealIP:      realIP,
		InternalIPs: internalIPs,
		Reason:      entry.Reason,
		TS:          time.Now().Unix(),
	}
	data, _ := json.Marshal(event)
	if err := w.rdb.Publish(ctx, "ban_events", data).Err(); err != nil {
		log.Printf("ban: PUBLISH ban_events: %v", err)
	}

	// Remove the entry from banned-ips.json so the proxy IP doesn't stay banned
	banFile := filepath.Join(w.volumesRoot, uuid, "banned-ips.json")
	w.removeFromBannedIPs(banFile, internalIP)

	log.Printf("ban: processed ban for real=%s internal=%s (uuid=%s)", realIP, internalIP, uuid)
}

func (w *Watcher) removeFromBannedIPs(path, ipToRemove string) {
	entries, err := readBannedIPs(path)
	if err != nil {
		log.Printf("warn: removeFromBannedIPs read %s: %v", path, err)
		return
	}

	filtered := entries[:0]
	for _, e := range entries {
		if e.IP != ipToRemove {
			filtered = append(filtered, e)
		}
	}

	data, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		log.Printf("warn: marshal banned-ips: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("warn: write %s: %v", path, err)
	}
}

func readBannedIPs(path string) ([]bannedEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []bannedEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return entries, nil
}
