package main

import (
	"fmt"
	"kubreed/pkg/libs"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

func formPiiAttackPayload(piiPercent int, attackPercent int, userPercent int) []string {
	count := make([]string, piiPercent+attackPercent+userPercent+1)
	pii := [5]string{"cc=5555555555554444", "ssn=234-90-2232", "PassportID=100003106", "itins=912-79-1234", "bankroutingnumber=133563585"}

	attack := [5]string{"d=${jndi:ldap://127.0.0.1/a}", "id=Holly%22%20UNION%20SELECT%20database(),2,3,4,5,6,7%20--+",
		"1;phpinfo()", "/../../../../etc/passwd", ">/><body/onload=alert()>"}

	user := [5]string{"username=test1", "user=test2", "username=kube1", "user=kube2", "username=test3"}

	j := 0
	for i := 0; i <= piiPercent+attackPercent+userPercent; i++ {
		if i < piiPercent {
			count[i] = pii[j]
		} else if (i >= piiPercent) && (i < piiPercent+attackPercent) {
			count[i] = attack[j]
		} else {
			count[i] = user[j]
		}
		j++

		if j == 5 {
			j = 0
		}
	}
	return count
}

func main() {

	configJSON := os.Getenv(libs.ConfigEnvVar)
	c, err := libs.GetConfigFromJSON(configJSON)
	PiiAttackPayload := formPiiAttackPayload(c.PiiPercent, c.AttackPercent, c.UserPercent)
	var url string
	if err != nil {
		log.Fatalf("ENV variable not set properly for configuration: %v", err)
		return
	}

	log.Printf("Config is: %#v", c)

	// prepare server
	for i := 0; i < c.APICount; i++ {
		endpoint := fmt.Sprintf("/api%d.txt", i)
		http.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
			randSleep := rand.Int63n((c.ResponseTime).Milliseconds())
			<-time.After(time.Duration(randSleep))
			w.Write([]byte("OK"))
			log.Printf("HTTPServer processed request from: %q", r.RemoteAddr)
		})
	}

	// launch client threads that talk to other servers
	go func() {
		reqCounter := 0
		reqCount := 0

		for {
			// We can add a select loop here and gracefully exit if needed
			for _, svc := range c.RemoteServices {
				for apiIter := 0; apiIter < c.APICount; apiIter++ {
					go func(svc string, apiIter int) {
						if reqCount >= 100 {
							reqCount = 0
						}
						if reqCount >= c.AttackPercent+c.PiiPercent+c.UserPercent {
							url = fmt.Sprintf("http://%s/api%d.txt", svc, apiIter)
						} else {
							url = fmt.Sprintf("http://%s/api%d.txt?%s", svc, apiIter, PiiAttackPayload[reqCount])
						}
						resp, err := http.Get(url)
						if err != nil {
							log.Printf("HTTPClient GET %q failed: %v", url, err)
							return
						}
						log.Printf("HTTPClient GET %q: %v", url, resp.Status)
						resp.Body.Close()

					}(svc, apiIter)
					reqCounter++

					if reqCounter == c.RPS {
						reqCounter = 0
						reqCount++
						<-time.After(time.Second)
						log.Print("---------------------")
					}
				}
			}
		}
	}()

	// Launch server and block forever
	log.Fatal(http.ListenAndServe(":80", nil))
}
