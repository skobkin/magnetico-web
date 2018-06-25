<?php

namespace App\Security;

use App\Api\V1\DTO\ApiResponse;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};
use Symfony\Component\Security\Core\Authentication\Token\{PreAuthenticatedToken, TokenInterface};
use Symfony\Component\Security\Core\Exception\{AuthenticationException, BadCredentialsException, CustomUserMessageAuthenticationException};
use Symfony\Component\Security\Core\User\UserProviderInterface;
use Symfony\Component\Security\Http\Authentication\{AuthenticationFailureHandlerInterface, SimplePreAuthenticatorInterface};
use Symfony\Component\Serializer\SerializerInterface;

class ApiTokenAuthenticator implements SimplePreAuthenticatorInterface, AuthenticationFailureHandlerInterface
{
    public const TOKEN_HEADER = 'api-token';

    /** @var SerializerInterface */
    private $serializer;

    public function __construct(SerializerInterface $serializer)
    {
        $this->serializer = $serializer;
    }

    /** Takes request data and creates token which will be ready to auth check */
    public function createToken(Request $request, $providerKey)
    {
        if (!($tokenKey = $request->headers->get(self::TOKEN_HEADER))) {
            throw new BadCredentialsException(sprintf('\'%s\' is invalid or not defined', self::TOKEN_HEADER));
        }

        return new PreAuthenticatedToken(
            'anon.',
            $tokenKey,
            $providerKey
        );
    }

    public function authenticateToken(TokenInterface $token, UserProviderInterface $userProvider, $providerKey)
    {
        if (!$userProvider instanceof ApiTokenUserProvider) {
            throw new \InvalidArgumentException(sprintf(
                'The user provider for providerKey = \'%s\' must be an instance of %s, %s given.',
                $providerKey,
                ApiTokenUserProvider::class,
                get_class($userProvider)
            ));
        }

        $apiTokenKey = $token->getCredentials();

        $user = $userProvider->loadUserByUsername($apiTokenKey);

        if (!$user) {
            throw new CustomUserMessageAuthenticationException(sprintf(
                'API token \'%s\' does not exist.', $apiTokenKey
            ));
        }

        return new PreAuthenticatedToken(
            $user,
            $apiTokenKey,
            $providerKey,
            $user->getRoles()
        );
    }

    public function onAuthenticationFailure(Request $request, AuthenticationException $exception): JsonResponse
    {
        // @todo Decouple with App\Api\V1\DTO
        $json = $this->serializer->serialize(
            new ApiResponse(null, JsonResponse::HTTP_UNAUTHORIZED, $exception->getMessage()),
            'json',
            ['groups' => ['api_v1']]
        );

        return new JsonResponse($json, JsonResponse::HTTP_UNAUTHORIZED,[], true);
    }


    public function supportsToken(TokenInterface $token, $providerKey)
    {
        return $token instanceof PreAuthenticatedToken && $token->getProviderKey() === $providerKey;
    }
}