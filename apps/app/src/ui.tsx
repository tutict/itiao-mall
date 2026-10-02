import type { ReactNode } from 'react';
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  type StyleProp,
  StyleSheet,
  Text,
  View,
  type ViewStyle,
} from 'react-native';

export const colors = {
  bg: '#f5f5f7',
  surface: '#ffffff',
  border: '#e5e5ea',
  text: '#1c1c1e',
  textMuted: '#6b7280',
  /**
   * 中国电商的习惯：涨价/金额/主行动用红，跌用绿。
   * 与欧美相反，这里保持红色做主色调。
   */
  primary: '#e4393c',
  primarySoft: '#fdecec',
  success: '#0f9d58',
  warning: '#b45309',
  warningSoft: '#fef3c7',
};

export function Screen({
  children,
  scroll = true,
  contentStyle,
}: {
  children: ReactNode;
  scroll?: boolean;
  contentStyle?: StyleProp<ViewStyle>;
}) {
  if (!scroll) {
    return <View style={[styles.screen, contentStyle]}>{children}</View>;
  }
  return (
    <ScrollView
      style={styles.screen}
      contentContainerStyle={[styles.screenContent, contentStyle]}
      keyboardShouldPersistTaps="handled"
    >
      {children}
    </ScrollView>
  );
}

export function Card({ children, style }: { children: ReactNode; style?: StyleProp<ViewStyle> }) {
  return <View style={[styles.card, style]}>{children}</View>;
}

export function Title({ children }: { children: ReactNode }) {
  return <Text style={styles.title}>{children}</Text>;
}

export function Muted({ children }: { children: ReactNode }) {
  return <Text style={styles.muted}>{children}</Text>;
}

export function Body({ children }: { children: ReactNode }) {
  return <Text style={styles.body}>{children}</Text>;
}

export function Price({ children, size = 18 }: { children: ReactNode; size?: number }) {
  return <Text style={[styles.price, { fontSize: size }]}>{children}</Text>;
}

export function Button({
  label,
  onPress,
  disabled = false,
  variant = 'primary',
  loading = false,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  variant?: 'primary' | 'secondary';
  loading?: boolean;
}) {
  const isDisabled = disabled || loading;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled: isDisabled, busy: loading }}
      onPress={onPress}
      disabled={isDisabled}
      style={({ pressed }) => [
        styles.button,
        variant === 'secondary' ? styles.buttonSecondary : styles.buttonPrimary,
        isDisabled && styles.buttonDisabled,
        pressed && !isDisabled && styles.buttonPressed,
      ]}
    >
      {loading ? (
        <ActivityIndicator color={variant === 'secondary' ? colors.text : '#fff'} />
      ) : (
        <Text style={[styles.buttonLabel, variant === 'secondary' && styles.buttonLabelSecondary]}>
          {label}
        </Text>
      )}
    </Pressable>
  );
}

export function Badge({
  text,
  tone = 'neutral',
}: {
  text: string;
  tone?: 'neutral' | 'warning' | 'success';
}) {
  const toneStyle =
    tone === 'warning'
      ? styles.badgeWarning
      : tone === 'success'
        ? styles.badgeSuccess
        : styles.badgeNeutral;
  return (
    <View style={[styles.badge, toneStyle]}>
      <Text style={styles.badgeText}>{text}</Text>
    </View>
  );
}

export function ErrorText({ children }: { children: ReactNode }) {
  return <Text style={styles.error}>{children}</Text>;
}

export function Loading({ label = '加载中' }: { label?: string }) {
  return (
    <View style={styles.loading}>
      <ActivityIndicator color={colors.primary} />
      <Muted>{label}</Muted>
    </View>
  );
}

export function Row({ children, style }: { children: ReactNode; style?: StyleProp<ViewStyle> }) {
  return <View style={[styles.row, style]}>{children}</View>;
}

export const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: colors.bg },
  screenContent: { padding: 16, gap: 12 },
  card: {
    backgroundColor: colors.surface,
    borderRadius: 12,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: colors.border,
    padding: 14,
    gap: 8,
  },
  title: { fontSize: 17, fontWeight: '600', color: colors.text },
  body: { fontSize: 15, color: colors.text },
  muted: { fontSize: 13, color: colors.textMuted },
  price: { color: colors.primary, fontWeight: '700' },
  button: {
    borderRadius: 10,
    paddingVertical: 12,
    paddingHorizontal: 16,
    alignItems: 'center',
    justifyContent: 'center',
    minHeight: 46,
  },
  buttonPrimary: { backgroundColor: colors.primary },
  buttonSecondary: { backgroundColor: colors.surface, borderWidth: 1, borderColor: colors.border },
  buttonDisabled: { opacity: 0.45 },
  buttonPressed: { opacity: 0.75 },
  buttonLabel: { color: '#fff', fontSize: 16, fontWeight: '600' },
  buttonLabelSecondary: { color: colors.text },
  badge: { borderRadius: 999, paddingHorizontal: 8, paddingVertical: 2, alignSelf: 'flex-start' },
  badgeNeutral: { backgroundColor: colors.bg },
  badgeWarning: { backgroundColor: colors.warningSoft },
  badgeSuccess: { backgroundColor: '#e6f4ea' },
  badgeText: { fontSize: 12, color: colors.text },
  error: { color: colors.primary, fontSize: 14 },
  loading: { paddingVertical: 32, alignItems: 'center', gap: 8 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 8 },
});
