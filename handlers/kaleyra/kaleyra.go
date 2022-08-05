package kaleyra

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/gocommon/httpx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/pkg/errors"
)

const (
	configAccountSID = "account_sid"
	configApiKey     = "api_key"
)

var (
	baseURL  = "https://api.kaleyra.io"
	urlRegex = regexp.MustCompile(`https?:\/\/(www\.)?[^\W][-a-zA-Z0-9@:%.\+~#=]{1,256}[^\W]\.[a-zA-Z()]{1,6}\b([-a-zA-Z0-9()@:%_\+.~#?&//=]*)`)
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("KWA"), "Kaleyra WhatsApp")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodGet, "receive", h.receiveMsg)
	s.AddHandlerRoute(h, http.MethodGet, "status", h.receiveStatus)
	return nil
}

type moMsgForm struct {
	CreatedAt string `name:"created_at"`
	Type      string `name:"type"`
	From      string `name:"from" validate:"required"`
	Name      string `name:"name"`
	Body      string `name:"body"`
	MediaURL  string `name:"media_url"`
}

type moStatusForm struct {
	ID     string `name:"id"     validate:"required"`
	Status string `name:"status" validate:"required"`
}

// receiveMsg is our HTTP handler function for incoming messages
func (h *handler) receiveMsg(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	form := &moMsgForm{}
	err := handlers.DecodeAndValidateForm(form, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// invalid type? ignore this
	if form.Type != "text" && form.Type != "image" && form.Type != "video" && form.Type != "voice" && form.Type != "document" {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, "ignoring request, unknown message type")
	}
	// check empty content
	if form.Body == "" && form.MediaURL == "" {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errors.New("no text or media"))
	}

	// build urn
	urn, err := urns.NewWhatsAppURN(form.From)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// parse created_at timestamp
	ts, err := strconv.ParseInt(form.CreatedAt, 10, 64)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid created_at: %s", form.CreatedAt))
	}

	// build msg
	date := time.Unix(ts, 0).UTC()
	msg := h.Backend().NewIncomingMsg(channel, urn, form.Body).WithReceivedOn(date).WithContactName(form.Name)

	if form.MediaURL != "" {
		msg.WithAttachment(form.MediaURL)
	}

	// write msg
	return handlers.WriteMsgsAndResponse(ctx, h, []courier.Msg{msg}, w, r)
}

var statusMapping = map[string]courier.MsgStatusValue{
	"0":         courier.MsgFailed,
	"sent":      courier.MsgWired,
	"delivered": courier.MsgDelivered,
	"read":      courier.MsgDelivered,
}

// receiveStatus is our HTTP handler function for outgoing messages statuses
func (h *handler) receiveStatus(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	form := &moStatusForm{}
	err := handlers.DecodeAndValidateForm(form, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// unknown status? ignore this
	msgStatus, found := statusMapping[form.Status]
	if !found {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, fmt.Sprintf("unknown status: %s", form.Status))
	}

	// msg not found? ignore this
	status := h.Backend().NewMsgStatusForExternalID(channel, form.ID, msgStatus)
	if status == nil {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, fmt.Sprintf("ignoring request, message %s not found", form.ID))
	}

	// write status
	return handlers.WriteMsgStatusAndResponse(ctx, h, channel, status, w, r)
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	start := time.Now()
	accountSID := msg.Channel().StringConfigForKey(configAccountSID, "")
	apiKey := msg.Channel().StringConfigForKey(configApiKey, "")

	if accountSID == "" || apiKey == "" {
		return nil, errors.New("no account_sid or api_key config")
	}

	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)

	sendURL := fmt.Sprintf("%s/v1/%s/messages", baseURL, accountSID)
	var logs []*courier.ChannelLog
	var kwaRes *httpx.Trace
	var kwaErr error

	// make multipart form requests if we have attachments, the kaleyra api doesn't supports media url nor media upload before send
	if len(msg.Attachments()) > 0 {
	attachmentsLoop:
		for i, attachment := range msg.Attachments() {
			_, attachmentURL := handlers.SplitAttachment(attachment)

			// download media
			req, _ := http.NewRequest(http.MethodGet, attachmentURL, nil)
			trace, err := handlers.MakeHTTPRequest(req)
			if err != nil {
				log := courier.NewChannelLogFromTrace("Media Fetch", msg.Channel(), msg.ID(), trace)
				logs = append(logs, log)
				kwaErr = err
				break
			}

			// create media part
			tokens := strings.Split(attachmentURL, "/")
			fileName := tokens[len(tokens)-1]
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)
			part, err := writer.CreateFormFile("media", fileName)
			_, err = io.Copy(part, bytes.NewReader(trace.ResponseBody))
			if err != nil {
				elapsed := time.Now().Sub(start)
				log := courier.NewChannelLogFromError("Media Send APPEND media Field Error", msg.Channel(), msg.ID(), elapsed, err)
				logs = append(logs, log)
				kwaErr = err
				break
			}

			// fill base values
			baseForm := h.newSendForm(msg.Channel(), "media", msg.URN().Path())
			if i == 0 {
				baseForm["caption"] = msg.Text()
			}
			for k, v := range baseForm {
				part, err := writer.CreateFormField(k)
				if err != nil {
					elapsed := time.Now().Sub(start)
					log := courier.NewChannelLogFromError(fmt.Sprintf("Media Send APPEND %s Field Error", k), msg.Channel(), msg.ID(), elapsed, err)
					logs = append(logs, log)
					kwaErr = err
					break attachmentsLoop
				}

				_, err = part.Write([]byte(v))
				if err != nil {
					elapsed := time.Now().Sub(start)
					log := courier.NewChannelLogFromError(fmt.Sprintf("Media Send APPEND %s Field Error", k), msg.Channel(), msg.ID(), elapsed, err)
					logs = append(logs, log)
					kwaErr = err
					break attachmentsLoop
				}
			}

			writer.Close()

			// send multipart form
			req, _ = http.NewRequest(http.MethodPost, sendURL, body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			kwaRes, kwaErr = handlers.MakeHTTPRequest(req)
		}
	} else {
		form := url.Values{}
		baseForm := h.newSendForm(msg.Channel(), "text", msg.URN().Path())
		baseForm["body"] = msg.Text()
		// checks if the message has a valid url to activate the preview
		if urlRegex.MatchString(msg.Text()) {
			baseForm["preview_url"] = "true"
		}
		for k, v := range baseForm {
			form.Set(k, v)
		}

		req, _ := http.NewRequest(http.MethodPost, sendURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		kwaRes, kwaErr = handlers.MakeHTTPRequest(req)
	}

	if kwaRes != nil {
		log := courier.NewChannelLogFromTrace("Message Sent", msg.Channel(), msg.ID(), kwaRes).WithError("Message Send Error", kwaErr)
		logs = append(logs, log)
	}
	// add logs to status
	for _, log := range logs {
		status.AddLog(log)
	}

	if kwaErr != nil {
		status.SetStatus(courier.MsgFailed)
		return status, nil
	}

	// record external id from the last sent msg request
	externalID, err := jsonparser.GetString(kwaRes.ResponseBody, "id")
	if err == nil {
		status.SetExternalID(externalID)
	}

	status.SetStatus(courier.MsgWired)
	return status, nil
}

func (h *handler) newSendForm(channel courier.Channel, msgType, toContact string) map[string]string {
	callbackDomain := channel.CallbackDomain(h.Server().Config().Domain)
	statusURL := fmt.Sprintf("https://%s/c/kwa/%s/status", callbackDomain, channel.UUID())

	return map[string]string{
		"api-key":      channel.StringConfigForKey(configApiKey, ""),
		"channel":      "WhatsApp",
		"from":         channel.Address(),
		"callback_url": statusURL,
		"type":         msgType,
		"to":           toContact,
	}
}
