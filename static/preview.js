// Library pre-listen: one shared <audio> element, per-row play/pause + skip buttons.
(function () {
    var audio = document.getElementById('preview-audio');
    if (!audio) return;

    var PLAY = '▶';  // ▶
    var PAUSE = '⏸'; // ⏸

    // Re-asserts button state; also runs on timeupdate so icons self-heal
    // after Datastar re-patches the track list mid-playback.
    function refresh() {
        var current = audio.dataset.track || '';
        var playing = current !== '' && !audio.paused && !audio.ended;
        document.querySelectorAll('.preview-btn').forEach(function (b) {
            b.textContent = (playing && b.dataset.track === current) ? PAUSE : PLAY;
        });
        document.querySelectorAll('.preview-fwd, .preview-stop').forEach(function (b) {
            b.classList.toggle('active', current !== '' && b.dataset.track === current);
        });
    }

    window.previewToggle = function (btn) {
        var id = btn.dataset.track;
        if (audio.dataset.track === id && !audio.paused) {
            audio.pause();
        } else if (audio.dataset.track === id && !audio.ended && audio.currentTime > 0) {
            audio.play();
        } else {
            audio.dataset.track = id;
            audio.src = '/api/library/preview?id=' + encodeURIComponent(id);
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
