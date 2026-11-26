package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"open_live_platform/internal/date"
	"open_live_platform/internal/transport/httpx"
)

func (a *Dates) sttSession(w http.ResponseWriter, r *http.Request, uid int64, active bool) (date.Session, bool) {
	channel, ok := dateID(w, r, "channel", 0)
	if !ok {
		return date.Session{}, false
	}
	session, err := a.Service.SessionFor(r.Context(), channel, uid, active)
	if err != nil {
		dateResponse(w, err, nil)
		return date.Session{}, false
	}
	return session, true
}

func (a *Dates) sttStart(w http.ResponseWriter, r *http.Request, uid int64) {
	session, ok := a.sttSession(w, r, uid, true)
	if !ok {
		return
	}
	var languages []string
	if !httpx.Decode(w, r, &languages) {
		return
	}
	language := r.Header.Get(sttLanguageHeader)
	if len(languages) == 0 {
		languages = dateLanguages(session, uid, language)
	}
	err := a.Subtitles.Start(r.Context(), session.Channel, uid, languages, false)
	if err == nil {
		a.updateLanguage(r.Context(), session, uid, language)
	}
	sttResponse(w, err, "4008", "rtt start error", "")
}

func (a *Dates) sttQuery(w http.ResponseWriter, r *http.Request, uid int64) {
	session, ok := a.sttSession(w, r, uid, false)
	if ok {
		status, err := a.Subtitles.Query(r.Context(), session.Channel)
		sttResponse(w, err, "4009", "rtt query error", status)
	}
}

func (a *Dates) sttUpdate(w http.ResponseWriter, r *http.Request, uid int64) {
	session, ok := a.sttSession(w, r, uid, true)
	if !ok {
		return
	}
	language := r.Header.Get(sttLanguageHeader)
	err := a.Subtitles.Update(r.Context(), session.Channel, uid, dateLanguages(session, uid, language))
	if err == nil {
		a.updateLanguage(r.Context(), session, uid, language)
	}
	sttResponse(w, err, "4019", "rtt update error", "SUCCESS")
}

func (a *Dates) sttStop(w http.ResponseWriter, r *http.Request, uid int64) {
	session, ok := a.sttSession(w, r, uid, false)
	if ok {
		sttResponse(w, a.Subtitles.Stop(r.Context(), session.Channel, uid), "4010", "rtt stop error", "")
	}
}

func dateLanguages(session date.Session, uid int64, language string) []string {
	anchor, audience := session.AnchorLanguage, session.AudienceLanguage
	if language != "" {
		if uid == session.AnchorID {
			anchor = language
		} else {
			audience = language
		}
	}
	var languages []string
	if anchor != "" {
		languages = append(languages, anchor)
	}
	if audience != "" && audience != anchor {
		languages = append(languages, audience)
	}
	return languages
}

func (a *Dates) updateLanguage(ctx context.Context, session date.Session, uid int64, language string) {
	if language != "" {
		if err := a.Service.SetLanguage(ctx, session, uid, language); err != nil {
			return
		}
		if uid == session.AnchorID {
			session.AnchorLanguage = language
		} else {
			session.AudienceLanguage = language
		}
	}
	for _, reader := range []int64{session.AnchorID, session.AudienceID} {
		nid, remote, remoteNID := session.Participant(reader)
		localLang, remoteLang := session.AnchorLanguage, session.AudienceLanguage
		if reader == session.AudienceID {
			localLang, remoteLang = remoteLang, localLang
		}
		a.push(ctx, reader, "stt_language", map[string]string{"uid": strconv.FormatInt(reader, 10), "nid": strconv.FormatUint(uint64(nid), 10), "lang": localLang,
			"remoteUid": strconv.FormatInt(remote, 10), "remoteNid": strconv.FormatUint(uint64(remoteNID), 10), "remoteLang": remoteLang})
	}
}
