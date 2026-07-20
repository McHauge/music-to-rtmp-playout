// Drag-and-drop reordering for the flow builder rundown (#flow-rundown).
// Sortable handles the dragging; on drop we renumber the position badges and
// POST the full id order to /api/flow/reorder. On any failure we reload to
// resync the view with the database.
(function () {
    'use strict';

    function initRundownDnD() {
        var ol = document.getElementById('flow-rundown');
        if (!ol || ol._sortable || typeof Sortable === 'undefined') return;
        ol._sortable = Sortable.create(ol, {
            handle: '.flow-drag',
            animation: 150,
            dataIdAttr: 'data-id',
            ghostClass: 'flow-ghost',
            filter: '.empty',
            onEnd: function () {
                ol.querySelectorAll('.flow-pos').forEach(function (el, i) {
                    el.textContent = i + 1;
                });
                var body = new URLSearchParams({
                    playlist_id: ol.dataset.playlistId,
                    ids: ol._sortable.toArray().join(','),
                });
                fetch('/api/flow/reorder', { method: 'POST', body: body })
                    .then(function (res) { if (!res.ok) location.reload(); })
                    .catch(function () { location.reload(); });
            },
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', initRundownDnD);
    } else {
        initRundownDnD();
    }
    // Datastar patches #flow-rundown in place (idiomorph), which keeps the
    // Sortable instance alive — but guard against outright replacement.
    new MutationObserver(initRundownDnD).observe(document.body, {
        childList: true,
        subtree: true,
    });
})();
