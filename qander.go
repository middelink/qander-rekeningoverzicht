package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
	"gopkg.in/gomail.v2"
)

const (
	urlSITE       = "https://www.qander.nl"
	urlLOGIN      = urlSITE + "/service/login.jsp"
	urlSTATEMENTS = urlSITE + "/service/secure/statements.jsp"
)

var (
	days     = flag.Int("days", 0, "How old can the statement be before we skip it (days)")
	all      = flag.Bool("all", false, "Download all statements (as opposed to the last)")
	username = flag.String("user", os.Getenv("QANDER_USER"), "Qander username to log in with (required)")
	password = flag.String("pass", os.Getenv("QANDER_PASS"), "Qander password to log in with (required)")
	smtp     = flag.String("smtp", "", "SMTPserver to send message over (e.g. smtp.iaf.nl:587) (required)")
	smtpUser = flag.String("smtp_user", os.Getenv("SMTP_USER"), "Optional SMTP username to log in with")
	smtpPass = flag.String("smtp_pass", os.Getenv("SMTP_PASS"), "Optional SMTP password to log in with")
	smtpTo   = flag.String("smtp_to", "", "Comma seperated list of email recipients (required)")
)

func main() {
	flag.Parse()
	if len(*username) == 0 || len(*password) == 0 || len(*smtp) == 0 || len(*smtpTo) == 0 {
		log.Fatal("One or more required flags are not given")
	}

	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
	}
	resp, err := client.Get(urlLOGIN)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nlogin resp=%#v\n\n", resp)
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}
	//fmt.Printf("login body=%s\n\n", body)

	// We look for 3 items in the login screen:
	//  - 'login': '<something>'
	//  - urlDone : '<something>',
	//  - recaptchaSitekey: '<something>'
	reLogin := regexp.MustCompile("'login'\\s*:\\s*'([^']*)'")
	reLogout := regexp.MustCompile("logout\\s*:\\s*'([^']*)'")
	reURLDone := regexp.MustCompile("urlDone\\s*:\\s*'([^']*)'")
	reRecaptcha := regexp.MustCompile("recaptchaSitekey\\s*:\\s*'([^']*)'")
	r := reLogin.FindSubmatch(body)
	if len(r) == 0 {
		log.Fatal("Unable to find 'login'")
	}
	fmt.Printf("login = %q\n", r[1])
	urlLogin := string(r[1])
	r = reLogout.FindSubmatch(body)
	if len(r) == 0 {
		log.Fatal("Unable to find 'logout'")
	}
	fmt.Printf("logout = %q\n", r[1])
	urlLogout := string(r[1])
	r = reURLDone.FindSubmatch(body)
	if len(r) == 0 {
		log.Fatal("Unable to find 'urlDone'")
	}
	fmt.Printf("URLDone = %q\n", r[1])
	r = reRecaptcha.FindSubmatch(body)
	if len(r) == 0 {
		log.Fatal("Unable to find 'recaptchaSitekey'")
	}
	fmt.Printf("recaptcha = %q\n\n", r[1])

	fmt.Println("Cookies: ")
	for _, cookie := range jar.Cookies(resp.Request.URL) {
		fmt.Printf("  %s: %s\n", cookie.Name, cookie.Value)
	}

	// Phase 2, json login.
	login := struct {
		Email    string  `json:"emailAddress"`
		Passwd   string  `json:"password"`
		Recatcha *string `json:"reCaptchaResponse"`
	}{
		Email:    *username,
		Passwd:   *password,
		Recatcha: nil}
	jsonValue, err := json.Marshal(login)
	if err != nil {
		log.Fatal(err)
	}
	resp, err = client.Post(urlSITE+urlLogin, "application/json", bytes.NewReader(jsonValue))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n(json)login resp=%#v\n\n", resp)
	body, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}

	// Defer the logout, thereby making sure we ALWAYS log out.
	defer func() {
		// Phase N. Logout
		resp, err = client.Post(urlSITE+urlLogout, "application/json", nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("\n(json)logout resp=%#v\n\n", resp)
		body, err = ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("logout body=%s\n\n", body)
	}()

	// Phase 3. Rekeningoverzicht
	resp, err = client.Get(urlSTATEMENTS)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nstatement resp=%#v\n\n", resp)
	body, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}
	//fmt.Printf("statement body=%s\n\n", body)
	// Looking for urls like
	// - /service/rest/statements/20170919/10594766/downloadPdf
	reStatements := regexp.MustCompile("/service/rest/statements/(?P<date>.*)/(?P<hash>.*)/downloadPdf")
	rs := reStatements.FindAllSubmatch(body, -1)
	if rs == nil {
		log.Fatal("Unable to find 'statements'")
	}
	fmt.Printf("statements = %q\n", rs)

	m := gomail.NewMessage()
	hasFile := 0
	for _, stmts := range rs {
		fmt.Printf("  date=%s, hash=%s\n", stmts[1], stmts[2])
		date, err := time.ParseInLocation("20060102", string(stmts[1]), time.Local)
		if err != nil {
			log.Printf("Unable to parse date %q, skipping", stmts[1])
			continue
		}
		// Check if date is not too old
		age := time.Now().Sub(date)
		if *days != 0 && age > time.Duration(*days)*24*time.Hour {
			fmt.Printf("Statement %q too old, skipping", stmts[1])
			continue
		}
		resp, err = client.Get(urlSITE + string(stmts[0]))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("\nstatement resp=%#v\n\n", resp)
		helper := func(r io.Reader) func(w io.Writer) error {
			return func(w io.Writer) error {
				_, err := io.Copy(w, r)
				return err
			}
		}(resp.Body)
		m.Attach("statement-"+string(stmts[1]), gomail.SetHeader(map[string][]string{"Content-Type": {"application/pdf"}}), gomail.SetCopyFunc(helper))
		hasFile++
	}
	if hasFile != 0 {
		m.SetHeader("From", "Qander Automailer <nobody@polyware.nl>")
		m.SetHeader("To", strings.Split(*smtpTo, ",")...)
		m.SetHeader("Subject", "Uw rekeningoverzicht van Qander")
		m.SetBody("text/plain", "Attached you will find the statements")

		// Add port 587 if it was not included
		_, _, err := net.SplitHostPort(*smtp)
		if err != nil {
			// For anything else than missing port, bail.
			if !strings.Contains(err.Error(), "missing port in address") {
				log.Fatalf("malformed address: %v", err)
			}
			*smtp = net.JoinHostPort(*smtp, "587")
		}
		host, portstr, err := net.SplitHostPort(*smtp)
		if err != nil {
			log.Fatal(err)
		}
		port, err := strconv.Atoi(portstr)
		if err != nil {
			log.Fatal(err)
		}
		d := gomail.NewDialer(host, port, *smtpUser, *smtpPass)
		// Send the email.
		if err := d.DialAndSend(m); err != nil {
			log.Fatal(err)
		}
	}

}
