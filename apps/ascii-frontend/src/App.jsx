import { useState } from 'react'

// Icons as simple SVG components
const SearchIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <circle cx="11" cy="11" r="8"></circle>
    <path d="m21 21-4.3-4.3"></path>
  </svg>
);

const SpinnerIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="spinner">
    <path d="M21 12a9 9 0 1 1-6.219-8.56"></path>
  </svg>
);

const CopyIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect width="14" height="14" x="8" y="8" rx="2" ry="2"></rect>
    <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"></path>
  </svg>
);

const DownloadIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path>
    <polyline points="7 10 12 15 17 10"></polyline>
    <line x1="12" x2="12" y1="15" y2="3"></line>
  </svg>
);

function App() {
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState(null);
  const [selectedView, setSelectedView] = useState('html');
  const [copied, setCopied] = useState(false);

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!input.trim()) return;

    setLoading(true);
    setError('');
    setResult(null);

    try {
      // Get the current environment
      const hostname = window.location.hostname;
      const isDevEnv = hostname.startsWith('dev.');
      const isStgEnv = hostname.startsWith('stg.');
      const env = isDevEnv ? 'dev' : isStgEnv ? 'stg' : 'prod';

      // Construct the enqueuer URL based on environment
      const enqueuerUrl = `https://${env}.enqueuer.fullstack.pw/add?queue=queue-${env}`;

      const response = await fetch(enqueuerUrl, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          content: input,
          id: `ascii-frontend-${Date.now()}`,
          headers: {
            source: 'ascii-frontend'
          }
        }),
      });

      if (!response.ok) {
        throw new Error(`Error: ${response.status} ${response.statusText}`);
      }

      const data = await response.json();
      setResult(data);
    } catch (err) {
      setError(err.message || 'Failed to generate ASCII art');
    } finally {
      setLoading(false);
    }
  };

  const copyToClipboard = () => {
    if (!result) return;

    const textToCopy = selectedView === 'html'
      ? result.image_ascii_html
      : result.image_ascii_text;

    navigator.clipboard.writeText(textToCopy)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      })
      .catch(err => {
        setError('Failed to copy to clipboard');
      });
  };

  const downloadAscii = () => {
    if (!result) return;

    const content = selectedView === 'html'
      ? result.image_ascii_html
      : result.image_ascii_text;
    const fileType = selectedView === 'html' ? 'html' : 'txt';
    const filename = `ascii-art-${Date.now()}.${fileType}`;

    const element = document.createElement('a');
    const file = new Blob([content], { type: selectedView === 'html' ? 'text/html' : 'text/plain' });
    element.href = URL.createObjectURL(file);
    element.download = filename;
    document.body.appendChild(element);
    element.click();
    document.body.removeChild(element);
  };

  const styles = {
    container: {
      minHeight: '100vh',
      background: 'linear-gradient(to bottom right, #1a202c, #2d3748)',
      color: 'white',
      padding: '1.5rem',
    },
    wrapper: {
      maxWidth: '64rem',
      margin: '0 auto',
    },
    header: {
      textAlign: 'center',
      marginBottom: '3rem',
    },
    title: {
      fontSize: '2.25rem',
      fontWeight: 'bold',
      marginBottom: '0.5rem',
    },
    subtitle: {
      color: '#a0aec0',
    },
    form: {
      marginBottom: '2rem',
    },
    inputContainer: {
      display: 'flex',
      flexDirection: 'column',
      gap: '1rem',
    },
    inputWrapper: {
      position: 'relative',
      flexGrow: 1,
    },
    input: {
      width: '100%',
      padding: '0.75rem 1rem',
      paddingLeft: '2.5rem',
      borderRadius: '0.5rem',
      backgroundColor: '#2d3748',
      border: '1px solid #4a5568',
      color: 'white',
      outline: 'none',
    },
    inputIconWrapper: {
      position: 'absolute',
      left: '0.75rem',
      top: '0.875rem',
      color: '#718096',
    },
    button: {
      padding: '0.75rem 1.5rem',
      backgroundColor: '#3182ce',
      borderRadius: '0.5rem',
      fontWeight: '500',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      cursor: 'pointer',
      border: 'none',
      color: 'white',
    },
    buttonDisabled: {
      opacity: '0.5',
      cursor: 'not-allowed',
    },
    error: {
      backgroundColor: 'rgba(221, 28, 28, 0.2)',
      border: '1px solid #c53030',
      color: 'white',
      padding: '1rem',
      borderRadius: '0.5rem',
      marginBottom: '2rem',
    },
    resultContainer: {
      backgroundColor: '#2d3748',
      border: '1px solid #4a5568',
      borderRadius: '0.5rem',
      overflow: 'hidden',
    },
    resultHeader: {
      display: 'flex',
      borderBottom: '1px solid #4a5568',
      padding: '0.75rem 1rem',
    },
    tabs: {
      display: 'flex',
      gap: '0.5rem',
    },
    tab: {
      padding: '0.375rem 0.75rem',
      borderRadius: '0.25rem',
      backgroundColor: '#4a5568',
      cursor: 'pointer',
      border: 'none',
      color: 'white',
    },
    activeTab: {
      backgroundColor: '#3182ce',
    },
    buttonGroup: {
      marginLeft: 'auto',
      display: 'flex',
      gap: '0.5rem',
    },
    iconButton: {
      padding: '0.375rem',
      backgroundColor: '#4a5568',
      borderRadius: '0.25rem',
      cursor: 'pointer',
      border: 'none',
      color: 'white',
    },
    asciiContainer: {
      padding: '1.5rem',
      backgroundColor: 'black',
      overflow: 'auto',
      maxHeight: '600px',
    },
    preContainer: {
      padding: '1.5rem',
      backgroundColor: 'black',
      fontFamily: 'monospace',
      whiteSpace: 'pre',
      overflow: 'auto',
      maxHeight: '600px',
    },
    imageSection: {
      padding: '1rem',
      borderTop: '1px solid #4a5568',
    },
    imageTitle: {
      fontWeight: '500',
      marginBottom: '0.5rem',
    },
    imageContainer: {
      maxHeight: '20rem',
      overflow: 'hidden',
      borderRadius: '0.25rem',
      backgroundColor: 'black',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
    },
    image: {
      maxWidth: '100%',
      maxHeight: '100%',
      objectFit: 'contain',
    },
    notification: {
      position: 'fixed',
      bottom: '1.5rem',
      right: '1.5rem',
      backgroundColor: '#2f855a',
      color: 'white',
      padding: '0.5rem 1rem',
      borderRadius: '0.5rem',
      boxShadow: '0 10px 15px -3px rgba(0, 0, 0, 0.1), 0 4px 6px -2px rgba(0, 0, 0, 0.05)',
    },
  };

  // Add media query for larger screens
  if (window.innerWidth >= 768) {
    styles.inputContainer = {
      ...styles.inputContainer,
      flexDirection: 'row',
    };
  }

  return (
    <div style={styles.container}>
      <div style={styles.wrapper}>
        <header style={styles.header}>
          <h1 style={styles.title}>ASCII Art Generator</h1>
          <p style={styles.subtitle}>Turn your descriptions into beautiful ASCII art</p>
        </header>

        <form onSubmit={handleSubmit} style={styles.form}>
          <div style={styles.inputContainer}>
            <div style={styles.inputWrapper}>
              <input
                type="text"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                placeholder="Enter a description (e.g., mountain landscape, cute cat, space shuttle)"
                style={styles.input}
                disabled={loading}
              />
              <div style={styles.inputIconWrapper}>
                <SearchIcon />
              </div>
            </div>
            <button
              type="submit"
              disabled={loading || !input.trim()}
              style={{
                ...styles.button,
                ...(loading || !input.trim() ? styles.buttonDisabled : {})
              }}
            >
              {loading ? (
                <>
                  <SpinnerIcon />
                  <span style={{ marginLeft: '0.5rem' }}>Generating...</span>
                </>
              ) : (
                'Generate ASCII Art'
              )}
            </button>
          </div>
        </form>

        {error && (
          <div style={styles.error}>
            <p>{error}</p>
          </div>
        )}

        {result && (
          <div style={styles.resultContainer}>
            <div style={styles.resultHeader}>
              <div style={styles.tabs}>
                <button
                  onClick={() => setSelectedView('html')}
                  style={{
                    ...styles.tab,
                    ...(selectedView === 'html' ? styles.activeTab : {})
                  }}
                >
                  HTML View
                </button>
                <button
                  onClick={() => setSelectedView('text')}
                  style={{
                    ...styles.tab,
                    ...(selectedView === 'text' ? styles.activeTab : {})
                  }}
                >
                  Text View
                </button>
              </div>
              <div style={styles.buttonGroup}>
                <button
                  onClick={copyToClipboard}
                  style={styles.iconButton}
                  title="Copy to clipboard"
                >
                  <CopyIcon />
                </button>
                <button
                  onClick={downloadAscii}
                  style={styles.iconButton}
                  title="Download file"
                >
                  <DownloadIcon />
                </button>
              </div>
            </div>

            {selectedView === 'html' ? (
              <div style={styles.asciiContainer} dangerouslySetInnerHTML={{ __html: result.image_ascii_html }} />
            ) : (
              <pre style={styles.preContainer}>
                {result.image_ascii_text}
              </pre>
            )}

            {result.image_url && (
              <div style={styles.imageSection}>
                <h3 style={styles.imageTitle}>Source Image</h3>
                <div style={styles.imageContainer}>
                  <img
                    src={result.image_url}
                    alt="Source for ASCII art"
                    style={styles.image}
                  />
                </div>
              </div>
            )}
          </div>
        )}

        {copied && (
          <div style={styles.notification}>
            Copied to clipboard!
          </div>
        )}
      </div>
    </div>
  );
}

export default App
