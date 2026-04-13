final List<String> textFileExtensions = [
  // Common Text File Extensions
  'txt',
  'text',
  'md',
  'rtf',
  'csv',
  'tsv',
  'json',
  'xml',
  'html',
  'htm',
  'css',
  'yaml',
  'yml',
  'ini',
  'cfg',
  'conf',
  'log',

  // Programming/Scripting Language Extensions
  'c',
  'h',
  'cpp',
  'hpp',
  'cc',
  'cxx',
  'java',
  'js',
  'ts',
  'py',
  'rb',
  'php',
  'pl',
  'sh',
  'bat',
  'cmd',
  'ps1',
  'vbs',
  'sql',
  'r',
  'go',
  'swift',
  'lua',
  'asm',
  'cs',
  'vb',
  'kt',
  'kts',
  'scala',
  'rs',
  'dart',
  'groovy',
  'dockerfile',

  // Web Development
  'jsx',
  'tsx',
  'vue',
  'asp',
  'aspx',
  'jsp',
  'ejs',
  'pug',
  'jade',

  // Document/Data Formats
  'svg',
  'tex',
  'bib',
  'properties',
  'diff',
  'patch',
  'toml',

  // Configuration/Development
  'gitignore',
  'htaccess',
  'env',
  'eslintrc',
  'dockerignore',
  'editorconfig',
  'Makefile',
  'mk',
];

final List<String> imageFileExtensions = [
  'jpg',
  'jpeg',
  'png',
  'gif',
  'bmp',
  'webp',
  'tiff',
  'svg',
  'ico',
  'jpe',
  'jfif',
  'heic',
  'heif',
  'avif',
  'apng',
];

final List<String> documentFileExtensions = [
  // Microsoft Office
  'doc',
  'docx',
  'xls',
  'xlsx',
  'ppt',
  'pptx',
  'vsd',
  'vsdx',
  'pub',
  'pdf',
];

/// Document extensions that get their own content-part type and are parsed
/// server-side by the docsupport package. Adding an entry here (and a
/// matching parser on the server) is all that's needed to support a new format.
const documentTypeExts = {'pdf', 'docx', 'xlsx', 'pptx'};

bool isDocumentType(String type) => documentTypeExts.contains(type);
