package longpoll_transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	lp_models "ovk-im/src/models/longpoll"
	longpoll "ovk-im/src/repo/redis"
	"ovk-im/src/transport/broadcaster"
)

func LongPollHandler(
	w http.ResponseWriter,
	r *http.Request,
	b *broadcaster.Broadcaster,
	lpRepo *longpoll.Repo,
) {
	query := r.URL.Query()
	if query.Get("health") != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	w.Header().Set("Content-Type", "application/json")

	key := query.Get("key")
	if key == "" {
		json.NewEncoder(w).Encode(lp_models.Envelope{Failed: 2})
		return
	}

	ts, err := strconv.ParseUint(query.Get("ts"), 10, 64)
	if err != nil {
		json.NewEncoder(w).Encode(lp_models.Envelope{Failed: 1})
		return
	}
	modeRaw, _ := strconv.ParseUint(query.Get("mode"), 10, 32)
	mode := uint32(modeRaw)
	versionStr := query.Get("version")
	if versionStr == "" {
		versionStr = "0"
	}
	version, _ := strconv.Atoi(versionStr)

	wait, _ := strconv.Atoi(query.Get("wait"))
	if wait <= 5 {
		wait = 30
	} else if wait > 90 {
		wait = 90
	}

	describedStr := query.Get("described")
	if describedStr == "" {
		describedStr = "0"
	}
	described, _ := strconv.Atoi(describedStr)

	config := lp_models.LPConfig{
		Version:   version,
		Mode:      mode,
		Described: described,
	}

	subjectID, err := lpRepo.GetUserIDByKey(r.Context(), key)
	if err != nil {
		json.NewEncoder(w).Encode(lp_models.Envelope{Failed: 2})
		return
	}

	packUpdates := func(events []lp_models.VKEvent) []json.RawMessage {
		res := make([]json.RawMessage, 0, len(events))
		for _, ev := range events {
			if _, ok := ev.(lp_models.MakingACallEvent); ok && !config.HasExtended() {
				continue
			}

			slice := ev.ToSlice(config)
			data, _ := json.Marshal(slice)
			res = append(res, json.RawMessage(data))
		}
		return res
	}

	notify := b.Subscribe(subjectID)
	defer b.Unsubscribe(subjectID, notify)

	updates, newTS, err := lpRepo.GetUpdates(r.Context(), subjectID, ts)
	if err == longpoll.ErrTsTooOld {
		json.NewEncoder(w).Encode(lp_models.Envelope{
			Failed: 1,
			TS:     newTS,
		})
		return
	}

	if len(updates) > 0 {
		currentPTS, _ := lpRepo.GetUserPTS(r.Context(), subjectID)

		resp := lp_models.Envelope{
			TS:      newTS,
			Updates: packUpdates(updates),
		}
		resp.PTS = currentPTS
		if resp.PTS == 0 {
			resp.PTS = newTS
		}

		json.NewEncoder(w).Encode(resp)
		return
	}

	select {
	case <-notify:
		freshUpdates, latestTS, _ := lpRepo.GetUpdates(r.Context(), subjectID, ts)
		currentPTS, _ := lpRepo.GetUserPTS(r.Context(), subjectID)

		resp := lp_models.Envelope{
			TS:      latestTS,
			Updates: packUpdates(freshUpdates),
		}
		resp.PTS = currentPTS
		if resp.PTS == 0 {
			resp.PTS = latestTS
		}

		json.NewEncoder(w).Encode(resp)

	case <-time.After(time.Duration(wait) * time.Second):
		currentTS, _ := lpRepo.GetUserTS(r.Context(), subjectID)
		if currentTS == 0 {
			currentTS = ts
		}

		json.NewEncoder(w).Encode(lp_models.Envelope{
			TS:      currentTS,
			PTS:     currentTS,
			Updates: []json.RawMessage{},
		})

	case <-r.Context().Done():
		return
	}
}
