package cache

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	ologger "github.com/kabanero-io/kabanero-operator/pkg/controller/logger"
	"github.com/kabanero-io/kabanero-operator/pkg/controller/utils/timer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var httpclog = ologger.NewOperatorlogger("controller.utils.cache.httpcache")

// Value in the cache map.  This contains the etag returned from the remote
// server, which is used on subsequent requests to use the cached data.
type cacheValue struct {
	etag     string
	date     string
	body     []byte
	lastUsed time.Time
}

// The cache is stored as a map.  We are storing the value as a struct
// instead of a pointer because multiple threads will be using the values
// concurrently.
var httpCache = make(map[string]cacheValue)

// Initialization mutex
var startPurgeTicker sync.Once

// The Duration at which a cache entry will be purged.
const purgeDuration = 12 * time.Hour

// The amount of time between cache purge ticker cycles
const tickerDuration = 30 * time.Minute

// Mutex for concurrent map access
var cacheLock sync.Mutex

// Returns the requested resource, either from the cache, or from the
// remote server.  The cache is not meant to be a "high performance" or
// "heavily concurrent" cache.
func GetFromCache(c client.Client, url string, skipCertVerify bool) ([]byte, error) {

	// Build the request.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	// See if the object is in the cache.  Drop the lock after adding the
	// header so we're not holding the lock around the HTTP request.
	cacheLock.Lock()
	cacheData, ok := httpCache[url]
	cacheLock.Unlock()
	if ok {
		req.Header.Add("If-None-Match", cacheData.etag)
		req.Header.Add("If-Modified-Since", cacheData.date)
	}

	// Drive the request. Certificate validation is not disabled by default.
	// Ignore the error from TLS config - if nil comes back, use the default.
	transport := &http.Transport{DisableCompression: true}
	tlsConfig, _ := GetTLSCConfig(c, skipCertVerify)

	transport.TLSClientConfig = tlsConfig

	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)

	// If something went horribly wrong, tell the user.  If we were using the
	// default TLS config, make that part of the error message.
	if err != nil {
		if tlsConfig == nil {
			return nil, fmt.Errorf("HTTP request error while using the default TLS configuration: %v", err.Error())
		}
		return nil, err
	}
	defer resp.Body.Close()

	// Check to see if we're going to use the cached data.
	if resp.StatusCode == http.StatusNotModified {
		httpclog.Info(fmt.Sprintf("Retrieved from cache: %v", url))

		// Update the last used time so the entry does not get purged.
		cacheData.lastUsed = time.Now()
		cacheLock.Lock()
		httpCache[url] = cacheData
		cacheLock.Unlock()

		return cacheData.body, nil
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(fmt.Sprintf("Could not retrieve the resource: %v. Http status code: %v", url, resp.StatusCode))
	}

	// We got some new data back.  Read it, and then see if we can cache it.
	r := resp.Body
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	etag := resp.Header.Get("ETag")
	date := resp.Header.Get("Date")

	// Re-lock the cache before either adding or removing the response from it.
	cacheLock.Lock()
	defer cacheLock.Unlock()
	if (len(etag) > 0) && (len(date) > 0) {
		// Before adding an entry to the cache, make sure the purge task is running.
		startPurgeTicker.Do(func() {
			timer.ScheduleWork(tickerDuration, purgeCache, purgeDuration)
		})
		httpCache[url] = cacheValue{etag: etag, date: date, body: b, lastUsed: time.Now()}
		httpclog.Info(fmt.Sprintf("Stored to cache: %v", url))
	} else {
		// Take the entry out of the map if it's already there.
		delete(httpCache, url)
	}

	return b, nil
}

// Purges the cache
func purgeCache(localPurgeDuration time.Duration) {
	cacheLock.Lock()
	defer cacheLock.Unlock()
	for key, _ := range httpCache {
		if time.Since(httpCache[key].lastUsed) > localPurgeDuration {
			httpclog.Info("Purging from cache: " + key)
			delete(httpCache, key)
		}
	}
}
