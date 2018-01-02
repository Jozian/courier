package dmark

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
	"github.com/pkg/errors"
)

var sendURL = "https://smsapi1.dmarkmobile.com/sms/"

// DMark supports up to 3 segment messages
const maxMsgLength = 453

func init() {
	courier.RegisterHandler(NewHandler())
}

type handler struct {
	handlers.BaseHandler
}

// NewHandler returns a new DMark Handler
func NewHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("DK"), "dmark")}
}

type receiveRequest struct {
	MSISDN    string `validate:"required" name:"msisdn"`
	Text      string `validate:"required" name:"text"`
	ShortCode string `validate:"required" name:"short_code"`
	TStamp    string `validate:"required" name:"tstamp"`
}

type statusRequest struct {
	ID     string `validate:"required" name:"id"`
	Status string `validate:"required" name:"status"`
}

var statusMapping = map[string]courier.MsgStatusValue{
	"1":  courier.MsgDelivered,
	"2":  courier.MsgErrored,
	"4":  courier.MsgSent,
	"8":  courier.MsgSent,
	"16": courier.MsgErrored,
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	err := s.AddHandlerRoute(h, "POST", "receive", h.ReceiveMessage)
	if err != nil {
		return err
	}
	return s.AddHandlerRoute(h, "POST", "status", h.StatusMessage)
}

// ReceiveMessage is our HTTP handler function for incoming messages
func (h *handler) ReceiveMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// get our params
	dkMsg := &receiveRequest{}
	err := handlers.DecodeAndValidateForm(dkMsg, r)
	if err != nil {
		return nil, err
	}

	// create our date from the timestamp "2017-10-26T15:51:32.906335+00:00"
	date, err := time.Parse("2006-01-02T15:04:05.999999-07:00", dkMsg.TStamp)
	if err != nil {
		return nil, fmt.Errorf("invalid tstamp: %s", dkMsg.TStamp)
	}

	// create our URN
	urn := urns.NewTelURNForCountry(dkMsg.MSISDN, channel.Country())

	// build our msg
	msg := h.Backend().NewIncomingMsg(channel, urn, dkMsg.Text).WithReceivedOn(date)

	// and finally queue our message
	err = h.Backend().WriteMsg(ctx, msg)
	if err != nil {
		return nil, err
	}

	return []courier.Event{msg}, courier.WriteMsgSuccess(ctx, w, r, []courier.Msg{msg})
}

// StatusMessage is our HTTP handler function for status updates
func (h *handler) StatusMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// get our params
	dkStatus := &statusRequest{}
	err := handlers.DecodeAndValidateForm(dkStatus, r)
	if err != nil {
		return nil, err
	}

	msgStatus, found := statusMapping[dkStatus.Status]
	if !found {
		return nil, fmt.Errorf("unknown status '%s', must be one of '1','2','4','8' or '16'", dkStatus.Status)
	}

	// write our status
	status := h.Backend().NewMsgStatusForExternalID(channel, dkStatus.ID, msgStatus)
	err = h.Backend().WriteMsgStatus(ctx, status)
	if err != nil {
		return nil, err
	}

	return []courier.Event{status}, courier.WriteStatusSuccess(ctx, w, r, []courier.MsgStatus{status})
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	// get our authentication token
	auth := msg.Channel().StringConfigForKey(courier.ConfigAuthToken, "")
	if auth == "" {
		return nil, fmt.Errorf("no authorization token set for DK channel")
	}

	callbackDomain := msg.Channel().CallbackDomain(h.Server().Config().Domain)
	dlrURL := fmt.Sprintf("https://%s%s%s/status?id=%s&status=%%s", callbackDomain, "/c/dk/", msg.Channel().UUID(), msg.ID().String())

	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	parts := handlers.SplitMsg(msg.Text(), maxMsgLength)
	for i, part := range parts {
		form := url.Values{
			"sender":   []string{strings.TrimLeft(msg.Channel().Address(), "+")},
			"receiver": []string{strings.TrimLeft(msg.URN().Path(), "+")},
			"text":     []string{part},
			"dlr_url":  []string{dlrURL},
		}

		req, err := http.NewRequest(http.MethodPost, sendURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Token %s", auth))
		rr, err := utils.MakeHTTPRequest(req)

		// record our status and log
		log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)
		status.AddLog(log)
		if err != nil {
			return status, nil
		}

		// grab the external id
		externalID, err := jsonparser.GetString([]byte(rr.Body), "sms_id")
		if err != nil {
			log.WithError("Message Send Error", errors.Errorf("unable to get sms_id from body"))
			return status, nil
		}

		// if this is our first message, record the external id
		if i == 0 {
			status.SetExternalID(externalID)
		}

		// this was wired successfully
		status.SetStatus(courier.MsgWired)
	}

	return status, nil
}
