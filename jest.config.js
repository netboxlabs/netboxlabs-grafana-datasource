// force timezone to UTC to allow tests to work regardless of local timezone
// generally used by snapshots, but can affect specific tests
process.env.TZ = 'UTC';
const { grafanaESModules, nodeModulesToTransform } = require('./.config/jest/utils');

module.exports = {
  // Jest configuration provided by Grafana scaffolding
  ...require('./.config/jest.config'),
  // @grafana/data 13.3 imports these ESM-only packages. Newer create-plugin
  // templates list them in .config/jest/utils.js; drop this override once
  // .config is updated.
  transformIgnorePatterns: [nodeModulesToTransform([...grafanaESModules, '@react-hookz/web', '@ver0/deep-equal'])],
};
