import { DataSourcePlugin } from '@grafana/data';
import { DataSource } from './datasource';
import { ConfigEditor } from './components/ConfigEditor';
import { QueryEditor } from './components/QueryEditor';
import { NetBoxQuery, NetBoxDataSourceOptions } from './types';

export const plugin = new DataSourcePlugin<DataSource, NetBoxQuery, NetBoxDataSourceOptions>(DataSource)
  .setConfigEditor(ConfigEditor)
  .setQueryEditor(QueryEditor);
