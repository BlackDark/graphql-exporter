package graphql

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/vinted/graphql-exporter/pkg/config"
)

func GraphqlQuery(query string) ([]byte, error) {
	// Prepare the request body
	params := url.Values{"query": {query}}
	body := strings.NewReader(params.Encode())

	// Prepare the request
	req, err := http.NewRequest(http.MethodPost, config.Config.GraphqlURL, body)
	if err != nil {
		return nil, fmt.Errorf("error creating HTTP request: %s", err)
	}

	// Add headers
	req.Header.Add("Authorization", config.Config.GraphqlAPIToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Send the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending HTTP request: %s", err)
	}
	defer resp.Body.Close()

	// Check response status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	// Read and return response body
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %s", err)
	}

	return bodyBytes, nil
}
