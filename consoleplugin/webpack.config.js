const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');
const path = require('path');
const { execSync } = require('child_process');
const webpack = require('webpack');

const gitCommit = (() => {
  try { return execSync('git rev-parse --short HEAD', { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim(); }
  catch { return 'unknown'; }
})();

module.exports = {
  entry: {},
  context: path.resolve(__dirname, 'src'),
  output: {
    path: path.resolve(__dirname, 'dist'),
    filename: '[name]-bundle.js',
    chunkFilename: '[name]-chunk.js',
  },
  resolve: {
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
  },
  module: {
    rules: [
      {
        test: /\.(ts|tsx)$/,
        use: 'ts-loader',
        exclude: /node_modules/,
      },
    ],
  },
  plugins: [
    new ConsoleRemotePlugin({
      pluginMetadata: require('./plugin.json'),
      validateExtensionIntegrity: false,
    }),
    new webpack.DefinePlugin({
      'process.env.PLUGIN_GIT_COMMIT': JSON.stringify(gitCommit),
    }),
  ],
  devServer: {
    port: 9001,
    static: './dist',
  },
};
