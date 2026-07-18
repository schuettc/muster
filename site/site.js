// Shared behavior for both muster.tools pages: left-TOC scroll-spy and the
// light/dark theme toggle. The scroll-spy derives its section-id list from
// the .toc anchors already on the page, so it works unchanged whichever
// subset of sections a given page has.
(function () {
  var links = document.querySelectorAll('.toc a');
  if (!links.length) { return; }
  var map = {};
  links.forEach(function (a) { map[a.getAttribute('href').slice(1)] = a; });
  var obs = new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting) {
        links.forEach(function (a) { a.classList.remove('active'); });
        var a = map[e.target.id];
        if (a) { a.classList.add('active'); }
      }
    });
  }, { rootMargin: '-40% 0px -55% 0px' });
  Object.keys(map).forEach(function (id) {
    var el = document.getElementById(id);
    if (el) { obs.observe(el); }
  });
})();

(function () {
  var tt = document.getElementById('tt');
  if (!tt) { return; }
  tt.addEventListener('click', function () {
    var cur = document.documentElement.getAttribute('data-theme');
    if (!cur) { cur = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'; }
    document.documentElement.setAttribute('data-theme', cur === 'dark' ? 'light' : 'dark');
  });
})();
