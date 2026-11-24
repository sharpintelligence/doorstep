package promquery

import "net/http"

func BearerAuth(read func() (string, error)) func(*http.Request) error {
	return func(req *http.Request) error {
		token, err := read()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

func BasicAuth(username string, read func() (string, error)) func(*http.Request) error {
	return func(req *http.Request) error {
		password, err := read()
		if err != nil {
			return err
		}
		req.SetBasicAuth(username, password)
		return nil
	}
}
