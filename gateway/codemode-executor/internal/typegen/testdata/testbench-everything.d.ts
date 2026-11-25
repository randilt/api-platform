declare const codemode: {
  everything: {
    /** Adds two numbers */
    add(args: { /** First number */ a: number; /** Second number */ b: number; }): Promise<unknown>;
    /** Echoes back the input */
    echo(args: { /** Message to echo */ message: string; }): Promise<unknown>;
    /** Reports the value of a header on the inbound HTTP request */
    headers(args: { /** Header name to read from the request */ name: string; }): Promise<unknown>;
  };
};
