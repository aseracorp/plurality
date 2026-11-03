// Liveness probe for healthchecks / orchestrators (autoheal, k8s, etc.).
	// Returns a real 200 JSON (not the SPA catch-all) so external watchers can
	// distinguish "HTTP stack is up" from a missing route. Deliberately
	// unauthenticated — healthcheckers have no session cookie.
	//
	// Also surfaces the git rev the binary was built from (BUILD_REV, baked in
	// by the dockerfile from CI's --build-arg). This makes the "new image, old
	// Plurality.bin" staleness bug immediately observable: if the running
	// /health BUILD_REV does not match the git SHA of master, the deployed
	// container is running a stale binary and should be re-pulled.
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rev := os.Getenv("BUILD_REV")
		if rev == "" || rev == "unknown" {
			rev = version.GetServerVersion()
			if rev == "" {
				rev = "unknown"
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "build_rev": rev})
	}).Methods("GET", "OPTIONS")