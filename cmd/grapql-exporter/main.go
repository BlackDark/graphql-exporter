package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"errors"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	cacheDir       = "/tmp/query-caches"
	queriesDir     = "queries"
	cacheExtension = ".json"
)

var (
	EXPORTER_LISTEN_ADDR         = getenv("EXPORTER_LISTEN_ADDR", "0.0.0.0:9199")
	EXPORTER_TLS_CERT_FILE       = getenv("EXPORTER_TLS_CERT_FILE", "")
	EXPORTER_TLS_KEY_FILE        = getenv("EXPORTER_TLS_KEY_FILE", "")
	EXPORTER_GRAPHQL_CONFIG_PATH = getenv("EXPORTER_GRAPHQL_CONFIG_PATH", "/config.json")
	EXPORTER_GRAPHQL_URL         = getenv("EXPORTER_GRAPHQL_URL", "")
	EXPORTER_GRAPHQL_AUTH        = getenv("EXPORTER_GRAPHQL_AUTH", "")
	EXPORTER_CACHE_MINUTES       = getenvInt("EXPORTER_CACHE_MINUTES", 60)
)

var (
	client          = &http.Client{Timeout: 20 * time.Second}
	cacheExpiration = parseDuration(fmt.Sprintf("%dm", EXPORTER_CACHE_MINUTES))
	cacheMutex      sync.RWMutex
	config          QueryPaths
)

var ErrEnvVarEmpty = errors.New("getenv: environment variable empty")

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		fmt.Println("Error parsing duration:", err)
	}
	return d
}

func getenv(key string, fallback string) string {
	if value := os.Getenv(key); len(value) > 0 {
		return value
	}
	return fallback
}

func getenvStr(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return v, ErrEnvVarEmpty
	}
	return v, nil
}

func getenvInt(key string, fallback int) int {
	s, err := getenvStr(key)
	if err != nil {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}

type QueryPath struct {
	URL          string `json:"url"`
	AuthKey      string `json:"authKey"`
	AuthValue    string `json:"authValue"`
	CacheMinutes int    `json:"cacheMinutes"`
}

//type QueryPaths map[string]QueryPath

type QueryPaths struct {
	QueryPaths map[string]QueryPath `json:"queryPaths"`
}

func loadConfig() error {
	file, err := os.ReadFile(EXPORTER_GRAPHQL_CONFIG_PATH)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}

	err = json.Unmarshal(file, &config)
	if err != nil {
		fmt.Println("Error decoding JSON:", err)
		return err
	}

	return nil
}

func main() {
	fmt.Printf("Env Config - Listen address: %s\n", EXPORTER_LISTEN_ADDR)
	fmt.Printf("Env Config - Config path: %s\n", EXPORTER_GRAPHQL_CONFIG_PATH)
	fmt.Printf("Env Config - Cache minutes: %d\n", EXPORTER_CACHE_MINUTES)
	err := loadConfig()

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loaded %d configurations.", len(config.QueryPaths))

	var keys []string
	for k := range config.QueryPaths {
		keys = append(keys, k)

		cachePath := filepath.Join(cacheDir, k)

		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			err := os.Mkdir(cachePath, os.ModePerm)

			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		}
	}
	fmt.Println("Map keys:", keys) // Output: [apple banana cherry]

	http.HandleFunc("/queries/", handleQuery)

	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// Returns some basic info about the exporter.
		w.Write([]byte("GraphQL exporter for Prometheus.\n"))
		w.Write([]byte("Exporter metrics available at /metrics.\n"))
		w.Write([]byte("Querying available at /queries/<queryfile>.\n\n"))
		w.Write([]byte(fmt.Sprintf("Copyright (c) %s Eduard Marbach\n", time.Now().Format("2006"))))
	})

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Returns metrics for the exporter itself.
		promhttp.Handler().ServeHTTP(w, r)
	})

	log.Printf("info: listening on http://%s", EXPORTER_LISTEN_ADDR)
	log.Fatalf("critical: %s", http.ListenAndServe(EXPORTER_LISTEN_ADDR, nil))
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")

	if len(parts) != 4 {
		fmt.Println("Not enough parts provided:", parts)
		http.Error(w, "Failed to read query file", http.StatusInternalServerError)
		return
	}

	configPath := parts[2]

	val, ok := config.QueryPaths[configPath]
	if !ok {
		http.Error(w, fmt.Sprintf("Config not provided for GraphQL client: %s", configPath), http.StatusInternalServerError)
		return
	}

	queryName := filepath.Base(r.URL.Path)
	queryPath := filepath.Join(queriesDir, configPath, queryName+".gql")
	cachePath := filepath.Join(cacheDir, configPath, queryName+cacheExtension)

	cachedData, err := readCachedData(cachePath)
	if err == nil && !isCacheExpired(cachePath) {
		fmt.Fprintf(w, string(cachedData))
		return
	}

	queryData, err := os.ReadFile(queryPath)
	if err != nil {
		fmt.Println("Failed to read query file:", err)
		http.Error(w, "Failed to read query file", http.StatusInternalServerError)
		return
	}

	//authToken := r.Header.Get("Authorization")
	result, err := executeGraphQLQuery(string(queryData), val.URL, val.AuthKey, val.AuthValue)
	if err != nil {
		fmt.Println("Failed to execute GraphQL query:", err)
		http.Error(w, "Failed to execute GraphQL query", http.StatusInternalServerError)
		return
	}

	err = writeCachedData(cachePath, result)
	if err != nil {
		fmt.Println("Failed to write cache:", err)
	}

	fmt.Printf("Refreshed cache for path: %s", queryPath)

	fmt.Fprintf(w, string(result))
}

func executeGraphQLQuery(query, url string, authKey string, authValue string) ([]byte, error) {
	reqBody, err := json.Marshal(map[string]string{
		"query": string(query),
	})

	if err != nil {
		fmt.Println("Error constructing request body:", err)
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(reqBody)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(authKey, authValue)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func readCachedData(path string) ([]byte, error) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func writeCachedData(path string, data []byte) error {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	return os.WriteFile(path, data, 0644)
}

func isCacheExpired(path string) bool {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()

	info, err := os.Stat(path)
	if err != nil {
		return true // Assume cache is expired if unable to get file info
	}

	expirationTime := info.ModTime().Add(cacheExpiration)
	return time.Now().After(expirationTime)
}
