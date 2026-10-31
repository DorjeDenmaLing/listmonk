// How long ago an ISO time was: "4 minutes ago".
export function denmaAgo(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  for (const [n, name] of [[86400, 'day'], [3600, 'hour'], [60, 'minute']]) {
    if (s >= n) {
      const v = Math.floor(s / n);
      return `${v} ${name}${v === 1 ? '' : 's'} ago`;
    }
  }
  return 'just now';
}
