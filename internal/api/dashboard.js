(function () {
  'use strict';

  var refreshInterval = 10000;
  var refreshTimer = 0;
  var refreshController = null;
  var refreshInFlight = false;
  var stopped = false;
  var refreshCard = document.querySelector('[data-refresh-card]');
  var refreshDetail = document.querySelector('[data-refresh-detail]');
  var liveStatus = document.getElementById('dashboard-refresh-status');

  function announce(message) {
    if (liveStatus) {
      liveStatus.textContent = message;
    }
  }

  function setRefreshState(failed) {
    if (refreshCard) {
      refreshCard.classList.toggle('refresh-error', failed);
    }
    if (refreshDetail) {
      refreshDetail.textContent = failed ? 'Update failed · retrying' : 'Every 10 seconds';
    }
  }

  function scheduleRefresh(delay) {
    window.clearTimeout(refreshTimer);
    if (!stopped && !document.hidden) {
      refreshTimer = window.setTimeout(refreshDashboard, delay);
    }
  }

  async function refreshDashboard() {
    if (stopped || document.hidden || refreshInFlight) {
      return;
    }

    var currentState = document.getElementById('dashboard-state');
    if (!currentState) {
      return;
    }

    refreshInFlight = true;
    currentState.setAttribute('aria-busy', 'true');
    refreshController = new AbortController();
    var requestTimeout = window.setTimeout(function () {
      refreshController.abort();
    }, 8000);

    try {
      var response = await window.fetch(window.location.href, {
        cache: 'no-store',
        credentials: 'same-origin',
        headers: {'X-Dashboard-Refresh': '1'},
        signal: refreshController.signal
      });
      if (!response.ok) {
        throw new Error('dashboard refresh returned HTTP ' + response.status);
      }

      var contentType = response.headers.get('Content-Type') || '';
      if (contentType.indexOf('text/html') === -1) {
        throw new Error('dashboard refresh returned an unexpected content type');
      }

      var nextDocument = new DOMParser().parseFromString(await response.text(), 'text/html');
      var nextState = nextDocument.getElementById('dashboard-state');
      if (!nextState) {
        throw new Error('dashboard refresh response did not contain resource state');
      }

      if (currentState.contains(document.activeElement)) {
        announce('Dashboard update deferred while a resource control is focused.');
        return;
      }

      var currentGeneratedAt = document.getElementById('dashboard-generated-at');
      var nextGeneratedAt = nextState.getAttribute('data-generated-at');
      currentState.replaceWith(nextState);
      if (currentGeneratedAt && nextGeneratedAt) {
        currentGeneratedAt.textContent = nextGeneratedAt;
      }
      setRefreshState(false);
      announce('Dashboard resource data updated.');
    } catch (error) {
      if (error.name !== 'AbortError' || !document.hidden) {
        setRefreshState(true);
        announce('Dashboard update failed. Existing resource data is still displayed.');
      }
    } finally {
      window.clearTimeout(requestTimeout);
      refreshController = null;
      refreshInFlight = false;
      var activeState = document.getElementById('dashboard-state');
      if (activeState) {
        activeState.removeAttribute('aria-busy');
      }
      scheduleRefresh(refreshInterval);
    }
  }

  document.addEventListener('click', function (event) {
    if (!(event.target instanceof Element)) {
      return;
    }
    var trigger = event.target.closest('[data-dashboard-refresh]');
    if (!trigger) {
      return;
    }
    event.preventDefault();
    window.clearTimeout(refreshTimer);
    refreshDashboard();
  });

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      window.clearTimeout(refreshTimer);
      if (refreshController) {
        refreshController.abort();
      }
      return;
    }
    refreshDashboard();
  });

  window.addEventListener('pagehide', function () {
    stopped = true;
    window.clearTimeout(refreshTimer);
    if (refreshController) {
      refreshController.abort();
    }
  });

  window.addEventListener('pageshow', function () {
    stopped = false;
    scheduleRefresh(refreshInterval);
  });

  scheduleRefresh(refreshInterval);
})();
