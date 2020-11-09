/**
The Caring Hamster - service wor working with SMS messages

Author: kolabse

Runing:

hamster <typeEnv>

Arguments:

typeEnv - type of runtime enviroment. Posssible values - dev, prod

./hamster dev

*/

package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"golang.org/x/time/rate"

	"github.com/fatih/color"
	"github.com/fiorix/go-smpp/smpp"
	"github.com/fiorix/go-smpp/smpp/pdu"
	"github.com/fiorix/go-smpp/smpp/pdu/pdufield"
	"github.com/fiorix/go-smpp/smpp/pdu/pdutext"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
)

var sResult []byte
var startTime time.Time
var connectionStatus string = "starting..."

// Status of SMS
const (
	StatusNew         = 0
	StatusSendWork    = 1
	StatusSendSuccess = 2
	StatusSendWait    = 3
	StatusSendError   = 4
	StatusSendTimeout = 5
	StatusDelivered   = 6
)

// Source of SMS
const (
	Sender = "<sender_name>"
)

// Status of SMS part
const (
	Delivered     = "DELIVRD"
	Expired       = "EXPIRED"
	Deleted       = "DELETED"
	Undeliverable = "UNDELIV"
	Accepted      = "ACCEPTD"
	Unknown       = "UNKNOWN"
	Rejected      = "REJECTD"
)

// Text of final
const (
	RSFINAL    = "<r:FINAL>"
	RSPROGRESS = "<r:PROGRESS>"
)

// Code for status of SMS part
const (
	DeliveredCode     = 1
	ExpiredCode       = 2
	DeletedCode       = 3
	UndeliverableCode = 4
	AcceptedCode      = 5
	UnknownCode       = 6
	RejectedCode      = 7
)

const typeEnvDev = "dev"
const typeEnvProd = "prod"

const noArgsMsg = "Required argument TypeEnv is missing. Posssible values - dev, prod"

//IncomingMessage struct
type IncomingMessage struct {
	Mobile string
	Text   string
	Source string
}

//SuccessResponse struct
type SuccessResponse struct {
	SmsID    string
	Message  string
	SendTime string
}

//StatusResponse struct
type StatusResponse struct {
	Status string
	Uptime string
}

//ErrorResponse struct
type ErrorResponse struct {
	Error string
}

// SmsParts db record
type SmsParts struct {
	ID          int       //`db:"id"`
	ClaimsSmsID string    //`db:"claims_sms_id"`
	MessageID   string    //`db:"message_id"`
	Status      int       //`db:"status"`
	SubmitDate  time.Time //`db:"submit_date"`
	DoneDate    time.Time //`db:"done_date"`
	Err         string    //`db:"err"`
	Text        string    //`db:"text"`
	CreatedAt   time.Time //`db:"created_at"`
	UpdatedAt   time.Time //`db:"updated_at"`
}

// DeliveryMessage object
type DeliveryMessage struct {
	ID         string `json:"id"`
	Sub        string `json:"sub"`
	Dlvrd      string `json:"dlvrd"`
	SubmitDate string `json:"submit_date"`
	DoneDate   string `json:"done_date"`
	Stat       string `json:"stat"`
	Err        string `json:"err"`
	Text       string `json:"text"`
}

func init() {
	startTime = time.Now()
}

func main() {

	smppAddr := ""
	smppPass := ""
	typeEnv := ""
	pgServer := ""
	pgPort := ""
	pgUser := ""
	pgUserPass := ""
	apiPort := ""
	var intervalBeforeReconnect time.Duration = 0

	printWelcome()

	if len(os.Args) > 1 {
		typeEnv = os.Args[1]
	} else {
		red := color.New(color.FgRed)
		red.Println(noArgsMsg)
		panic("Please set required parameters")
	}
	if typeEnv == typeEnvProd {
		smppAddr = "<prod_smpp_server_name>"
		smppPass = "<prod_smpp_pass>"
		pgServer = "<prod_db_server>"
		pgPort = "<prod_db_port>"
		pgUser = "<prod_db_user>"
		pgUserPass = "<prod_db_user_pass>"
		apiPort = "<prod_api_port>"
		intervalBeforeReconnect = 30
	} else {
		log.Printf("DEV MODE")
		smppAddr = "<dev_smpp_server_name>"
		smppPass = "<dev_smpp_pass>"
		pgServer = "<dev_db_server>"
		pgPort = "<dev_db_port>"
		pgUser = "<dev_db_user>"
		pgUserPass = "<dev_db_user_pass>"
		apiPort = "<dev_api_port>"
		intervalBeforeReconnect = 2
	}

	time.Sleep(intervalBeforeReconnect * time.Second)

	dbURL := "postgres://" + pgUser + ":" + pgUserPass + "@" + pgServer + ":" + pgPort + "/<schema_name>?sslmode=disable"

	db, err := gorm.Open("postgres", dbURL)
	if err != nil {
		panic("Failed to connect database" + err.Error())
	}
	defer db.Close()
	db.SingularTable(true)

	var smsPartFound []SmsParts
	var smsPartMessageID string

	// func for work with incoming message
	f := func(p pdu.Body) {
		switch p.Header().ID {
		case pdu.DeliverSMID:
			// when we get message with delivery status
			f := p.Fields()
			rs := convertMessageToArray(f["short_message"].String())
			smsPartMessageID = rs.ID
			log.Printf("Try to find part with mID:%s", smsPartMessageID)
			db.Where(&SmsParts{MessageID: smsPartMessageID}).Where("message_id = ?", smsPartMessageID).Limit(1).Find(&smsPartFound)
			for _, smsPart := range smsPartFound {
				log.Printf("Part with id:%s", smsPart.ClaimsSmsID)
				smsSubmitDate := convertSMPPTimestampToPpostgrsqlDatetime(rs.SubmitDate)
				smsDoneDate := convertSMPPTimestampToPpostgrsqlDatetime(rs.DoneDate)
				smsErr := rs.Err
				smsText := rs.Text
				smsStatus := getPartStatus(rs.Stat)
				db.Model(&smsPart).Updates(map[string]interface{}{
					"submit_date": smsSubmitDate,
					"done_date":   smsDoneDate,
					"err":         smsErr,
					"text":        smsText,
					"status":      smsStatus,
				})
			}
		}
	}
	lm := rate.NewLimiter(rate.Limit(10), 1) // Max rate of 10/s.
	tx := &smpp.Transceiver{
		Addr:        smppAddr + ":11111",
		User:        "website",
		Passwd:      smppPass,
		Handler:     f,  // Handle incoming SM or delivery receipts.
		RateLimiter: lm, // Optional rate limiter.
	}
	// Create persistent connection.
	conn := tx.Bind()
	go func() {
		for c := range conn {
			log.Println("SMPP connection status:", c.Status())
			connectionStatus = c.Status().String()
		}
	}()

	http.HandleFunc("/messageSend", func(w http.ResponseWriter, r *http.Request) {
		//get fields from form
		decoder := json.NewDecoder(r.Body)
		var message IncomingMessage
		err := decoder.Decode(&message)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte((err.Error())))
		}

		smsMobile := message.Mobile
		smsText := message.Text
		smsSource := message.Source
		smsID := getUUID()

		log.Printf("Sending sms from %s, with text \"%s\", to number %s", smsSource, smsText, smsMobile)

		if isNumeric(smsMobile) {
			if len(smsMobile) == 11 {
				if smsText != "" {
					if smsSource != "" {
						parts, err := tx.SubmitLongMsg(&smpp.ShortMessage{
							Src:      smsSource,
							Dst:      smsMobile,
							Text:     pdutext.UCS2(smsText),
							Validity: 10 * time.Minute,
							Register: pdufield.FinalDeliveryReceipt,
						})
						if err == smpp.ErrNotConnected {
							time.Sleep(60000 * time.Millisecond)
							conn = tx.Bind()
						}
						if err != nil {
							log.Printf("Unable to connect, with error: %s", err.Error())
							time.Sleep(60000 * time.Millisecond)
							conn = tx.Bind()
						}
						for index, sm := range parts {
							msgid := sm.RespID()
							if msgid == "" {
								log.Fatalf("pdu does not contain msgid: %#v", sm.Resp())
							} else {
								log.Printf("Sended message index %d, msgid: %q, for sms with id:%s", index, msgid, smsID)
								ct := time.Now()
								currentTime := formatToSQLTimestamp(ct)
								log.Printf(currentTime)
								var smsPart = SmsParts{MessageID: msgid, ClaimsSmsID: smsID, CreatedAt: ct}
								db.Create(&smsPart)
							}
						}
						sResult, _ = json.Marshal(&SuccessResponse{SmsID: smsID, Message: "Message was send", SendTime: formatToSQLTimestamp(time.Now())})
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, string(sResult[:]))
					} else {
						w.WriteHeader(http.StatusBadRequest)
						w.Header().Set("Content-Type", "application/json")
						w.Write([]byte("Message field \"source\" is empty"))
					}
				} else {
					w.WriteHeader(http.StatusBadRequest)
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte("Message field \"text\" is empty"))
				}
			} else {
				w.WriteHeader(http.StatusBadRequest)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("Message field \"mobile\" is wrong length"))
			}
		} else {
			w.WriteHeader(http.StatusBadRequest)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("Message field \"mobile\" is empty or not number"))
		}

	})
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		serviceStatus, _ := json.Marshal(&StatusResponse{Status: connectionStatus, Uptime: shortDur(uptime())})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, string(serviceStatus[:]))
	})
	log.Fatal(http.ListenAndServe(":"+apiPort, nil))
}

func isNumeric(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// parse incoming message textt with delivery info
func convertMessageToArray(rs string) DeliveryMessage {
	rs = "[" + rs + "]"
	rs = strings.Replace(rs, "[id:", "{\"id\": \"", -1)
	rs = strings.Replace(rs, " sub:", "\",\"sub\": \"", -1)
	rs = strings.Replace(rs, " dlvrd:", "\",\"dlvrd\": \"", -1)
	rs = strings.Replace(rs, " submit date:", "\",\"submit_date\": \"", -1)
	rs = strings.Replace(rs, " done date:", "\",\"done_date\": \"", -1)
	rs = strings.Replace(rs, " stat:", "\",\"stat\": \"", -1)
	rs = strings.Replace(rs, " err:", "\",\"err\": \"", -1)
	rs = strings.Replace(rs, " text:", "\",\"text\": \"", -1)
	rs = strings.Replace(rs, "]", "\"}", -1)
	log.Println(rs)
	var message DeliveryMessage
	err := json.Unmarshal([]byte(rs), &message)
	if err != nil {
		log.Fatal(err)
	}
	return message
}

func convertSMPPTimestampToPpostgrsqlDatetime(ts string) time.Time {
	currentTime, err := time.Parse("060102150405", ts)
	failOnError(err, "Time parse faild")
	return currentTime
}

func getPartStatus(stat string) int {
	result := 0
	switch stat {
	case Delivered:
		result = DeliveredCode
	case Expired:
		result = ExpiredCode
	case Deleted:
		result = DeletedCode
	case Undeliverable:
		result = UndeliverableCode
	case Accepted:
		result = AcceptedCode
	case Unknown:
		result = UnknownCode
	case Rejected:
		result = RejectedCode
	}
	return result
}

func formatToSQLTimestamp(time time.Time) string {
	return time.Format("2006-01-02 15:04:05")
}

func getUUID() string {
	newUUID, _ := uuid.NewUUID()
	return newUUID.String()
}

func printWelcome() {
	log.Printf("        _           _")
	log.Printf("      (`-`;-\"```\"-;`-`)")
	log.Printf("       \\.'         './")
	log.Printf("       /             \\")
	log.Printf("       ;   0     0   ;")
	log.Printf("      /| =         = |\\")
	log.Printf("     ; \\   '._Y_.'   / ;")
	log.Printf("    ;   `-._ \\|/ _.-'   ;")
	log.Printf("   ;        `\"\"\"`        ;")
	log.Printf("   ;    `\"\"-.   .-\"\"`    ;")
	log.Printf("   /;  '--._ \\ / _.--   ;\\")
	log.Printf("  :  `.   `/|| ||\\`   .'  :")
	log.Printf("   '.  '-._       _.-'   .'        jgs")
	log.Printf("   (((-'`  `\"\"\"\"\"`   `'-)))")
	log.Printf("____________________________")
	log.Printf(" ")
	log.Printf("Hello! Mr. Caring Hamster with you!")
	log.Printf("Wait for smsgate connection...")
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

func uptime() time.Duration {
	return time.Since(startTime)
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
