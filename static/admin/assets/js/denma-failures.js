// denma: a campaign's failed sends (cmd/denma_retries.go,
// partials/denma/failures.html): resending a finished campaign to them.
// Spread into the campaign page's component.
import { api } from './main.js';
import * as u from './utils.js';

export function denmaFailures() {
  return {
    async denmaResend(id, num) {
      const who = `${num} ${num === 1 ? 'person' : 'people'}`;
      if (!(await u.confirm(`Send this campaign again to the ${who} it couldn't send to? No one else gets it.`))) {
        return;
      }
      const data = await api('campaigns', `/denma/campaigns/${id}/resend`, 'POST');
      const n = data.subscribers;
      u.reload({ message: `Resending to ${n} ${n === 1 ? 'person' : 'people'}`, variant: 'success' });
    },
  };
}
