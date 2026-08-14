import React from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { ConfigEditor, FAST_PAGING_TOOLTIP } from './ConfigEditor';
import { NetBoxDataSourceOptions } from '../types';

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
