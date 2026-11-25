declare const codemode: {
  weather: {
    /** Get today's forecast for a city. */
    get_forecast(args: { /** city name */ city: string; }): Promise<{ city: string; condition: string; tempC: number; }>;
  };
};
