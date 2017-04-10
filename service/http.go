package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/jcmturner/gokrb5/GSSAPI"
	"github.com/jcmturner/gokrb5/crypto"
	"github.com/jcmturner/gokrb5/iana/errorcode"
	"github.com/jcmturner/gokrb5/iana/keyusage"
	"github.com/jcmturner/gokrb5/keytab"
	"github.com/jcmturner/gokrb5/messages"
	"github.com/jcmturner/gokrb5/types"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// The response on successful authentication always has this header. Capturing as const so we don't have marshaling and encoding overhead.
	SPNEGO_NegTokenResp_Krb_Accept_Completed = "Negotiate oRQwEqADCgEAoQsGCSqGSIb3EgECAg=="
	SPNEGO_NegTokenResp_Reject               = "Negotiate oQcwBaADCgEC"
)

// SPNEGO Kerberos HTTP handler wrapper
func SPNEGOKRB5Authenticate(f http.HandlerFunc, ktab keytab.Keytab, l *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
		if len(s) != 2 || s[0] != "Negotiate" {
			w.Header().Set("WWW-Authenticate", "Negotiate")
			w.WriteHeader(401)
			w.Write([]byte("Unauthorised.\n"))
			return
		}
		b, err := base64.StdEncoding.DecodeString(s[1])
		if err != nil {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO error in base64 decoding negotiation header: %v", r.RemoteAddr, err))
			return
		}
		isInit, nt, err := GSSAPI.UnmarshalNegToken(b)
		if err != nil || !isInit {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO negotiation token is not a NegTokenInit: %v", r.RemoteAddr, err))
			return
		}
		nInit := nt.(GSSAPI.NegTokenInit)
		if !nInit.MechTypes[0].Equal(GSSAPI.MechTypeOID_Krb5) {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO OID of MechToken is not of type KRB5", r.RemoteAddr))
			return
		}
		var mt GSSAPI.MechToken
		err = mt.Unmarshal(nInit.MechToken)
		if err != nil {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO error unmarshaling MechToken: %v", r.RemoteAddr, err))
			return
		}
		if !mt.IsAPReq() {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - MechToken does not contain an AP_REQ - KRB_AP_ERR_MSG_TYPE", r.RemoteAddr))
			return
		}
		err = mt.APReq.Ticket.DecryptEncPart(ktab)
		if err != nil {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO error decrypting the service ticket provided: %v", r.RemoteAddr, err))
			return
		}
		ab, err := crypto.DecryptEncPart(mt.APReq.Authenticator, mt.APReq.Ticket.DecryptedEncPart.Key, keyusage.AP_REQ_AUTHENTICATOR)
		if err != nil {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO error decrypting the authenticator provided: %v", r.RemoteAddr, err))
			return
		}
		var a types.Authenticator
		err = a.Unmarshal(ab)
		if err != nil {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO error unmarshalling the authenticator: %v", r.RemoteAddr, err))
			return
		}
		if ok, err := validateAPREQ(a, mt.APReq); ok {
			ctx := r.Context()
			ctx = context.WithValue(ctx, "cname", a.CName.GetPrincipalNameString())
			ctx = context.WithValue(ctx, "crealm", a.CRealm)
			ctx = context.WithValue(ctx, "authenticated", true)
			w.Header().Set("WWW-Authenticate", SPNEGO_NegTokenResp_Krb_Accept_Completed)
			f(w, r.WithContext(ctx))
		} else {
			rejectSPNEGO(w, l, fmt.Sprintf("%v - SPNEGO Kerberos authentication failed: %v", r.RemoteAddr, err))
			return
		}
	}
}

func validateAPREQ(a types.Authenticator, APReq messages.APReq) (bool, error) {
	// Check CName in Authenticator is the same as that in the ticket
	if !a.CName.Equal(APReq.Ticket.DecryptedEncPart.CName) {
		err := messages.NewKRBError(APReq.Ticket.SName, APReq.Ticket.Realm, errorcode.KRB_AP_ERR_BADMATCH, "CName in Authenticator does not match that in service ticket")
		return false, err
	}
	// TODO client address check
	//The addresses in the ticket (if any) are then
	//searched for an address matching the operating-system reported
	//address of the client.  If no match is found or the server insists on
	//ticket addresses but none are present in the ticket, the
	//KRB_AP_ERR_BADADDR error is returned.

	// Check the clock skew between the client and the service server
	ct := a.CTime.Add(time.Duration(a.Cusec) * time.Microsecond)
	t := time.Now().UTC()
	// Hardcode 5 min max skew. May want to make this configurable
	d := time.Duration(5) * time.Minute
	if t.Sub(ct) > d || ct.Sub(t) > d {
		err := messages.NewKRBError(APReq.Ticket.SName, APReq.Ticket.Realm, errorcode.KRB_AP_ERR_SKEW, fmt.Sprintf("Clock skew with client too large. Greater than %v seconds", d))
		return false, err
	}

	// Check for replay
	rc := GetReplayCache(d)
	if rc.IsReplay(d, APReq.Ticket.SName, a) {
		err := messages.NewKRBError(APReq.Ticket.SName, APReq.Ticket.Realm, errorcode.KRB_AP_ERR_REPEAT, "Replay detected")
		return false, err
	}

	// Check for future tickets or invalid tickets
	if APReq.Ticket.DecryptedEncPart.StartTime.Sub(t) > d || types.IsFlagSet(&APReq.Ticket.DecryptedEncPart.Flags, types.Invalid) {
		err := messages.NewKRBError(APReq.Ticket.SName, APReq.Ticket.Realm, errorcode.KRB_AP_ERR_TKT_NYV, "Service ticket provided is not yet valid")
		return false, err
	}

	// Check for expired ticket
	if t.Sub(APReq.Ticket.DecryptedEncPart.EndTime) > d {
		err := messages.NewKRBError(APReq.Ticket.SName, APReq.Ticket.Realm, errorcode.KRB_AP_ERR_TKT_EXPIRED, "Service ticket provided has expired")
		return false, err
	}
	return true
}

func rejectSPNEGO(w http.ResponseWriter, l *log.Logger, logMsg string) {
	if l != nil {
		l.Println(logMsg)
	}
	w.Header().Set("WWW-Authenticate", SPNEGO_NegTokenResp_Reject)
	w.WriteHeader(401)
	w.Write([]byte("Unauthorised.\n"))
}
