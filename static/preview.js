// Library pre-listen: one shared <audio> element, per-row play/pause + skip buttons.
(function () {
    var audio = document.getElementById('preview-audio');
    if (!audio) return;

    var PLAY = '▶';  // ▶
    var PAUSE = '⏸'; // ⏸
    var STOP = '⏹';  // ⏹ — shown instead of pause on buttons with data-stop

    // Re-asserts button state; also runs on timeupdate so icons self-heal
    // after Datastar re-patches the track list mid-playback.
    function refresh() {
        var current = audio.dataset.track || '';
        var playing = current !== '' && !audio.paused && !audio.ended;
        document.querySelectorAll('.preview-btn').forEach(function (b) {
            var isCurrent = playing && b.dataset.track === current;
            // cue buttons keep the play glyph — clicking restarts, never stops
            b.textContent = isCurrent && !('cue' in b.dataset) ? ('stop' in b.dataset ? STOP : PAUSE) : PLAY;
            b.classList.toggle('active', isCurrent);
        });
        document.querySelectorAll('.preview-fwd, .preview-stop').forEach(function (b) {
            b.classList.toggle('active', current !== '' && b.dataset.track === current);
        });
    }

    window.previewToggle = function (btn) {
        var id = btn.dataset.track;
        if ('cue' in btn.dataset) { // data-cue: every click restarts from the top (hot cue)
            if (audio.dataset.track !== id) {
                audio.dataset.track = id;
                audio.src = btn.dataset.src || ('/api/library/preview?id=' + encodeURIComponent(id));
            }
            audio.currentTime = 0;
            audio.play();
            refresh();
            return;
        }
        var stopStyle = 'stop' in btn.dataset; // data-stop: stop & reset instead of pause/resume
        if (audio.dataset.track === id && !audio.paused) {
            if (stopStyle) {
                window.previewStop(btn);
                return;
            }
            audio.pause();
        } else if (!stopStyle && audio.dataset.track === id && !audio.ended && audio.currentTime > 0) {
            audio.play();
        } else {
            audio.dataset.track = id;
            audio.src = btn.dataset.src || ('/api/library/preview?id=' + encodeURIComponent(id));
            audio.play();
        }
        refresh();
    };

    window.previewStop = function (btn) {
        if (audio.dataset.track !== btn.dataset.track) return;
        audio.pause();
        audio.removeAttribute('src');
        audio.load();
        audio.dataset.track = '';
        refresh();
    };

    window.previewForward = function (btn) {
        if (audio.dataset.track === btn.dataset.track && audio.src) {
            audio.currentTime += 10;
        }
    };

    ['play', 'pause', 'ended', 'timeupdate'].forEach(function (ev) {
        audio.addEventListener(ev, refresh);
    });
})();
