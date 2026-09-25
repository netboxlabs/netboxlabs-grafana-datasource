import React from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { ConfigEditor, FAST_PAGING_TOOLTIP, MODE_TOOLTIP } from './ConfigEditor';
import { NetBoxDataSourceOptions } from '../types';

// @grafana/ui's Select menu (via ScrollIndicators) uses IntersectionObserver
// to decide when to show scroll shadows; jsdom doesn't implement it, so opening
// a Select's dropdown throws without this stub.
class IntersectionObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = IntersectionObserverStub;

function makeOptions(jsonData: Partial<NetBoxDataSourceOptions> = {}, overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    uid: 'ds-config-editor',
    name: 'NetBox',
    type: 'netboxlabs-datasource',
    typeName: 'NetBox',
    typeLogoUrl: '',
    access: 'proxy',
    url: '',
    user: '',
    database: '',
    basicAuth: false,
    basicAuthUser: '',
    isDefault: false,
    readOnly: false,
    withCredentials: false,
    jsonData,
    secureJsonFields: {},
    ...overrides,
  } as any;
}

function setup(jsonData: Partial<NetBoxDataSourceOptions> = {}, overrides: Record<string, unknown> = {}) {
  const onOptionsChange = jest.fn();
  render(<ConfigEditor options={makeOptions(jsonData, overrides)} onOptionsChange={onOptionsChange} />);
  return { onOptionsChange };
}

// The data-sources LIST reads each row's address from the settings' TOP-LEVEL
// url, while the real setting lives in jsonData.url. Nothing in the UI shows
// that mirror, so without these tests it can be deleted with every test still
// green and the only symptom is a list row with no address.
describe('data-sources list address', () => {
  it('mirrors an edited URL into the top-level field', () => {
    const { onOptionsChange } = setup({ url: 'https://old.example.com' });
    fireEvent.change(screen.getByPlaceholderText('https://netbox.example.com'), {
      target: { value: 'https://netbox.example.com' },
    });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({
        url: 'https://netbox.example.com',
        jsonData: expect.objectContaining({ url: 'https://netbox.example.com' }),
      })
    );
  });

  // Every datasource configured before the mirror existed has its address in
  // jsonData.url and an empty top-level url, so the on-edit mirror alone only
  // ever fixes new datasources — or ones whose URL someone retyped.
  it('backfills the top-level field for a datasource saved before the mirror existed', () => {
    const { onOptionsChange } = setup({ url: 'https://netbox.example.com' }, { url: '' });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({
        url: 'https://netbox.example.com',
        jsonData: expect.objectContaining({ url: 'https://netbox.example.com' }),
      })
    );
  });

  it('stays quiet when the mirror is already correct', () => {
    const { onOptionsChange } = setup({ url: 'https://netbox.example.com' }, { url: 'https://netbox.example.com' });
    expect(onOptionsChange).not.toHaveBeenCalled();
  });

  it('stays quiet with nothing to mirror', () => {
    const { onOptionsChange } = setup({}, { url: '' });
    expect(onOptionsChange).not.toHaveBeenCalled();
  });

  // A top-level url that already holds something else was put there by
  // something other than this mirror; overwriting it on mere page open would
  // be a settings change nobody asked for. The user's next URL edit re-mirrors.
  // The guard above is only worth anything against a parent that actually
  // applies the change and re-renders, which is what Grafana's settings page
  // does. A jest.fn() that swallows the update would never expose a loop.
  it('backfills once against a parent that applies the update', () => {
    const onOptionsChange = jest.fn();
    function Parent() {
      const [options, setOptions] = React.useState(
        makeOptions({ url: 'https://netbox.example.com' }, { url: '' }) as any
      );
      return (
        <ConfigEditor
          options={options}
          onOptionsChange={(next: any) => {
            onOptionsChange(next);
            setOptions(next);
          }}
        />
      );
    }
    render(<Parent />);
    expect(onOptionsChange).toHaveBeenCalledTimes(1);
    expect(onOptionsChange).toHaveBeenCalledWith(expect.objectContaining({ url: 'https://netbox.example.com' }));
  });

  it('leaves a differing top-level url alone', () => {
    const { onOptionsChange } = setup({ url: 'https://netbox.example.com' }, { url: 'https://proxy.example.com' });
    expect(onOptionsChange).not.toHaveBeenCalled();
  });
});

// Fast paging is a trade, not an improvement: it costs the user row order and
// match counts. A datasource that has never been told about it must not be
// making that trade, which is why the assertion is on the DEFAULT rather than
// only on the toggle.
describe('fast paging setting', () => {
  it('is off for a datasource that has never set it', () => {
    setup();
    expect(screen.getByRole('switch', { name: /fast paging/i })).not.toBeChecked();
  });

  it('is off when other advanced settings are configured', () => {
    setup({ tlsSkipVerify: true, timeoutSeconds: 60 });
    expect(screen.getByRole('switch', { name: /fast paging/i })).not.toBeChecked();
  });

  // The trade lives in the switch's own tooltip. It used to be repeated as a
  // paragraph below the field, which said the same thing twice and broke the
  // layout of the Advanced section.
  //
  // Asserted against the exported constant rather than the DOM: Grafana mounts
  // tooltip text only on hover and jsdom does not reproduce that, so a DOM
  // assertion here would pass for the wrong reason. This checks the copy, which
  // is the part that can regress silently.
  it('states both halves of the trade in the tooltip', () => {
    expect(FAST_PAGING_TOOLTIP).toMatch(/ID order/i);
    expect(FAST_PAGING_TOOLTIP).toMatch(/totals are unavailable/i);
    expect(FAST_PAGING_TOOLTIP).toMatch(/[Aa]lert rules are unaffected/i);
  });

  it('renders the switch with a tooltip icon', () => {
    setup();
    expect(screen.getByText('Fast paging').querySelector('[data-testid="info-circle"]')).toBeTruthy();
  });

  it('turns on only when the user asks', () => {
    const { onOptionsChange } = setup();
    fireEvent.click(screen.getByRole('switch', { name: /fast paging/i }));
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ fastPagingNoTotals: true }) })
    );
  });

  it('reflects an enabled setting', () => {
    setup({ fastPagingNoTotals: true });
    expect(screen.getByRole('switch', { name: /fast paging/i })).toBeChecked();
  });
});

// Without these fields the mode is reachable only through provisioning or by
// editing datasource settings by hand, which is how it shipped in the first
// draft. They also have to be conditional: the NetBox API token is never sent
// in replica-cache mode, and showing it implies a credential the backend does
// not use — the same confusion that made Save & Test fail on a correctly
// configured cache datasource.
describe('replica-cache mode', () => {
  // One set of connection fields for both modes: the URL and API token name
  // whichever service the mode reads from. The instance ID is the one field
  // only the replica needs.
  it('asks for URL and API token in NetBox mode, and no instance ID', () => {
    setup();
    expect(screen.getByLabelText('URL')).toBeInTheDocument();
    expect(screen.getByLabelText(/API token/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/NetBox instance ID/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Replica cache/i)).not.toBeInTheDocument();
  });

  it('asks for the same URL and API token in replica-cache mode, plus the instance ID', () => {
    setup({ mode: 'replica-cache' });
    expect(screen.getByLabelText('URL')).toBeInTheDocument();
    expect(screen.getByLabelText(/API token/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/NetBox instance ID/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Replica cache/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/NetBox URL/i)).not.toBeInTheDocument();
  });

  it('hints the service in the placeholders rather than in the labels', () => {
    setup({ mode: 'replica-cache' });
    expect(screen.getByPlaceholderText('https://<id>.replica-cache.example.com')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('ff_…')).toBeInTheDocument();
  });

  it('writes the URL to jsonData.url and the list address in cache mode too', () => {
    const { onOptionsChange } = setup({ mode: 'replica-cache' });
    fireEvent.change(screen.getByLabelText('URL'), { target: { value: 'https://cache.example.com' } });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({
        url: 'https://cache.example.com',
        jsonData: expect.objectContaining({ url: 'https://cache.example.com' }),
      })
    );
  });

  it('writes the instance ID into jsonData', () => {
    const { onOptionsChange } = setup({ mode: 'replica-cache' });
    fireEvent.change(screen.getByLabelText(/NetBox instance ID/i), { target: { value: 'nb-123' } });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ netboxId: 'nb-123' }) })
    );
  });

  // The token lands in secureJsonData under the one key both modes read, or
  // it is stored in the clear.
  it('stores the cache token as the API token secret', () => {
    const { onOptionsChange } = setup({ mode: 'replica-cache' });
    fireEvent.change(screen.getByLabelText(/API token/i), { target: { value: 'ff_secret' } });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ secureJsonData: expect.objectContaining({ apiToken: 'ff_secret' }) })
    );
    const call = onOptionsChange.mock.calls[0][0];
    expect(call.jsonData.apiToken).toBeUndefined();
  });

  // Max data age only means something for a backend that reports one, and it
  // must reach jsonData, where the backend reads it.
  it('offers Max data age in replica-cache mode and writes it to jsonData', () => {
    const { onOptionsChange } = setup({ mode: 'replica-cache' });
    fireEvent.change(screen.getByLabelText(/Max data age/i), { target: { value: '15m' } });
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ maxDataAge: '15m' }) })
    );
  });

  it('does not offer Max data age in NetBox mode', () => {
    setup();
    expect(screen.queryByLabelText(/Max data age/i)).not.toBeInTheDocument();
  });

  // One URL and one token for both modes means a switch would otherwise keep
  // the old service's address and credential: a stored NetBox token pointed at
  // the replica the moment the URL is edited, or a freshly entered replica
  // token sent to the NetBox host that the URL still names. Switching mode is
  // switching service, so both are cleared and must be re-entered.
  it('clears the stored token and the URL when the mode changes', async () => {
    const onOptionsChange = jest.fn();
    render(
      <ConfigEditor
        options={
          {
            ...makeOptions(
              { mode: 'netbox', url: 'https://netbox.example.com' },
              { url: 'https://netbox.example.com' }
            ),
            secureJsonFields: { apiToken: true },
            secureJsonData: { apiToken: 'nbt_unsaved' },
          } as any
        }
        onOptionsChange={onOptionsChange}
      />
    );
    const mode = screen.getByLabelText('Mode');
    fireEvent.keyDown(mode, { key: 'ArrowDown' });
    fireEvent.click(await screen.findByText('Replica cache'));

    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({
        url: '',
        jsonData: expect.objectContaining({ mode: 'replica-cache', url: '' }),
        secureJsonFields: expect.objectContaining({ apiToken: false }),
        secureJsonData: expect.objectContaining({ apiToken: '' }),
      })
    );
  });

  // The differences between the modes live in docs/REPLICA-CACHE.md and in the
  // query editor at the point of failure, not in a warning label on the
  // connection form.
  it('keeps the mode copy free of capability caveats', () => {
    expect(MODE_TOOLTIP).not.toMatch(/annotations|topology|IP enrichment|cannot|label/i);
  });
});

// The URL and Browser URL fields sit in the same place in both modes, so
// switching Mode does not shuffle the form under the cursor. They are shared
// settings, not mode-specific ones: the mode-specific field follows them.
describe('config field order', () => {
  const labelsInOrder = (container: HTMLElement) =>
    Array.from(container.querySelectorAll('label'))
      .map((l) => l.textContent?.trim())
      .filter((t): t is string => Boolean(t));

  it('keeps the shared connection fields directly after Mode in both modes', () => {
    const netbox = render(<ConfigEditor options={makeOptions({})} onOptionsChange={jest.fn()} />).container;
    const netboxOrder = labelsInOrder(netbox).slice(0, 3);

    const cache = render(
      <ConfigEditor options={makeOptions({ mode: 'replica-cache' })} onOptionsChange={jest.fn()} />
    ).container;
    const cacheOrder = labelsInOrder(cache).slice(0, 3);

    expect(netboxOrder).toEqual(cacheOrder);
    expect(netboxOrder[0]).toMatch(/Mode/i);
    expect(netboxOrder[1]).toBe('URL');
    expect(netboxOrder[2]).toMatch(/Browser URL/i);
  });
});

// Fast paging is a NetBox-only trade: the replica-cache backend has no cursor
// walk to opt into, so the switch would be a control that changes nothing.
describe('ConfigEditor fast paging visibility', () => {
  it('hides the fast paging switch in replica-cache mode', () => {
    setup({ mode: 'replica-cache' });
    expect(screen.queryByText('Fast paging')).not.toBeInTheDocument();
  });

  it('shows it in NetBox mode', () => {
    setup({});
    expect(screen.getByText('Fast paging')).toBeInTheDocument();
  });
});
